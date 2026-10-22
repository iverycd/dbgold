package com.dbgold.oscar;

import java.io.BufferedInputStream;
import java.io.BufferedOutputStream;
import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.io.StringReader;
import java.io.InputStream;
import java.io.Reader;
import java.math.BigDecimal;
import java.nio.charset.StandardCharsets;
import java.nio.charset.CodingErrorAction;
import java.nio.CharBuffer;
import java.nio.ByteBuffer;
import java.sql.BatchUpdateException;
import java.sql.Connection;
import java.sql.DatabaseMetaData;
import java.sql.DriverManager;
import java.sql.ParameterMetaData;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.ResultSetMetaData;
import java.sql.Types;
import java.sql.SQLException;
import java.sql.Statement;
import java.sql.Timestamp;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.FutureTask;
import java.util.concurrent.atomic.AtomicLong;

/**
 * A deliberately dependency-free JDBC bridge. Standard output is reserved for
 * framed protocol messages; diagnostics go to standard error.
 */
public final class BridgeMain {
    private static final int VERSION = 1;
    private static final int RESPONSE = 0x80;
    private static final int MAX_FRAME = 64 * 1024 * 1024;

    private static final int HELLO = 1;
    private static final int OPEN_SESSION = 2;
    private static final int PING = 3;
    private static final int LIST_SCHEMAS = 4;
    private static final int EXEC = 5;
    private static final int QUERY_INT64 = 6;
    private static final int BATCH_BEGIN = 7;
    private static final int BATCH_ADD = 8;
    private static final int BATCH_COMMIT = 9;
    private static final int BATCH_ABORT = 10;
    private static final int CLOSE_SESSION = 11;
    private static final int CANCEL = 12;
    private static final int SHUTDOWN = 13;
    private static final int DESCRIBE_TABLE = 14;
    private static final int CURSOR_OPEN = 15;
    private static final int CURSOR_FETCH = 16;
    private static final int CURSOR_CLOSE = 17;

    private static final int NULL = 0;
    private static final int BOOL = 1;
    private static final int INT64 = 2;
    private static final int UINT64 = 3;
    private static final int FLOAT64 = 4;
    private static final int DECIMAL = 5;
    private static final int STRING = 6;
    private static final int BYTES = 7;
    private static final int DATE = 8;
    private static final int TIME = 9;
    private static final int TIMESTAMP = 10;

    private final DataInputStream input = new DataInputStream(new BufferedInputStream(System.in));
    private final DataOutputStream output = new DataOutputStream(new BufferedOutputStream(System.out));
    private final Object outputLock = new Object();
    private final ExecutorService workers = Executors.newCachedThreadPool();
    private final Map<Long, Future<?>> requests = new ConcurrentHashMap<Long, Future<?>>();
    private final Map<Long, Statement> activeStatements = new ConcurrentHashMap<Long, Statement>();
    private final Map<Long, Session> sessions = new ConcurrentHashMap<Long, Session>();
    private final Map<Long, Batch> batches = new ConcurrentHashMap<Long, Batch>();
    private final Map<Long, Cursor> cursors = new ConcurrentHashMap<Long, Cursor>();
    private final Map<Long, Cursor> activeCursors = new ConcurrentHashMap<Long, Cursor>();
    private final AtomicLong nextCursor = new AtomicLong(1);
    private final AtomicLong nextSession = new AtomicLong(1);
    private final AtomicLong nextBatch = new AtomicLong(1);
    private volatile boolean running = true;

    public static void main(String[] args) {
        try {
            Class.forName("com.oscar.Driver");
            new BridgeMain().run();
        } catch (Throwable error) {
            System.err.println("Oscar JDBC bridge startup failed: " + safeMessage(error));
            System.exit(2);
        }
    }

    private void run() throws IOException {
        while (running) {
            final byte[] frame;
            try {
                int length = input.readInt();
                if (length <= 0 || length > MAX_FRAME) {
                    throw new IOException("invalid request frame size " + length);
                }
                frame = new byte[length];
                input.readFully(frame);
            } catch (EOFException eof) {
                break;
            }
            final DataInputStream body = new DataInputStream(new ByteArrayInputStream(frame));
            int version = body.readUnsignedByte();
            final int op = body.readUnsignedByte();
            final long requestId = body.readLong();
            if (version != VERSION) {
                writeError(requestId, "", 0, "protocol version mismatch");
                continue;
            }
            if (op == CANCEL) {
                cancelRequest(body.readLong());
                writeSuccess(requestId, null);
                continue;
            }
            if (op == SHUTDOWN) {
                closeEverything();
                writeSuccess(requestId, null);
                running = false;
                continue;
            }
			FutureTask<Void> future = new FutureTask<Void>(new Runnable() {
                @Override public void run() {
                    try {
                        handle(requestId, op, body);
                    } finally {
                        requests.remove(requestId);
                    }
                }
			}, null);
            requests.put(requestId, future);
			workers.execute(future);
        }
        closeEverything();
    }

    private void handle(long requestId, int op, DataInputStream body) {
        try {
            switch (op) {
                case HELLO:
                    writeSuccess(requestId, new BodyWriter() {
                        @Override public void write(DataOutputStream out) throws IOException {
                            writeString(out, "com.oscar.Driver");
							writeString(out, driverVersion());
                        }
                    });
                    return;
                case OPEN_SESSION:
                    openSession(requestId, body);
                    return;
                case PING:
                    ping(requestId, body.readLong());
                    return;
                case LIST_SCHEMAS:
                    listSchemas(requestId, body.readLong());
                    return;
                case EXEC:
                    execute(requestId, body.readLong(), readString(body));
                    return;
                case QUERY_INT64:
                    queryInt64(requestId, body.readLong(), readString(body));
                    return;
                case BATCH_BEGIN:
                    beginBatch(requestId, body.readLong(), readString(body));
                    return;
                case BATCH_ADD:
                    addBatch(requestId, body.readLong(), body);
                    return;
                case BATCH_COMMIT:
                    commitBatch(requestId, body.readLong());
                    return;
                case BATCH_ABORT:
                    abortBatch(requestId, body.readLong());
                    return;
                case DESCRIBE_TABLE:
                    describeTable(requestId, body.readLong(), readString(body), readString(body));
                    return;
                case CURSOR_OPEN:
                    openCursor(requestId, body.readLong(), readString(body));
                    return;
                case CURSOR_FETCH:
                    fetchCursor(requestId, body.readLong(), body.readInt(), body.readInt());
                    return;
                case CURSOR_CLOSE:
                    closeCursor(body.readLong());
                    writeSuccess(requestId, null);
                    return;
                case CLOSE_SESSION:
                    closeSession(requestId, body.readLong());
                    return;
                default:
                    throw new IOException("unsupported operation " + op);
            }
        } catch (Throwable error) {
            writeThrowable(requestId, error);
        }
    }

    private void openSession(final long requestId, DataInputStream body) throws Exception {
        String url = readString(body);
        String username = readString(body);
        String password = readString(body);
        String schema = readString(body);
        int maxOpen = body.readInt();
        int maxIdle = body.readInt();
        long lifetimeMs = body.readLong();
        int loginTimeoutMs = body.readInt();
        int socketTimeoutMs = body.readInt();
        long id = nextSession.getAndIncrement();
        Session session = new Session(url, username, password, schema, maxOpen, maxIdle,
                lifetimeMs, loginTimeoutMs, socketTimeoutMs);
		try {
			session.pool.test();
		} catch (Throwable error) {
			session.pool.close();
			throw error;
		}
        sessions.put(id, session);
        final long sessionId = id;
        writeSuccess(requestId, new BodyWriter() {
            @Override public void write(DataOutputStream out) throws IOException { out.writeLong(sessionId); }
        });
    }

    private Session session(long id) throws SQLException {
        Session result = sessions.get(id);
        if (result == null) throw new SQLException("Oscar JDBC session is closed", "08003");
        return result;
    }

    private void ping(long requestId, long sessionId) throws Exception {
        session(sessionId).pool.test();
        writeSuccess(requestId, null);
    }

    private void listSchemas(long requestId, long sessionId) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        List<String> names = new ArrayList<String>();
        try {
            DatabaseMetaData metadata = holder.connection.getMetaData();
            ResultSet rows = metadata.getSchemas();
            try {
                while (rows.next()) {
                    String name = rows.getString("TABLE_SCHEM");
                    if (name != null && !name.isEmpty()) names.add(name);
                }
            } finally {
                rows.close();
            }
        } finally {
            s.pool.release(holder);
        }
        Collections.sort(names);
        final List<String> result = names;
        writeSuccess(requestId, new BodyWriter() {
            @Override public void write(DataOutputStream out) throws IOException {
                out.writeInt(result.size());
                for (String name : result) writeString(out, name);
            }
        });
    }

    private void execute(long requestId, long sessionId, String sql) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        Statement statement = null;
        try {
            statement = holder.connection.createStatement();
            activeStatements.put(requestId, statement);
            statement.execute(sql);
        } finally {
            activeStatements.remove(requestId);
            closeQuietly(statement);
            s.pool.release(holder);
        }
        writeSuccess(requestId, null);
    }

    private void queryInt64(final long requestId, long sessionId, String sql) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        Statement statement = null;
        ResultSet rows = null;
        long value;
        try {
            statement = holder.connection.createStatement();
            activeStatements.put(requestId, statement);
            rows = statement.executeQuery(sql);
            if (!rows.next()) throw new SQLException("query returned no rows");
            value = rows.getLong(1);
        } finally {
            activeStatements.remove(requestId);
            closeQuietly(rows);
            closeQuietly(statement);
            s.pool.release(holder);
        }
        final long result = value;
        writeSuccess(requestId, new BodyWriter() {
            @Override public void write(DataOutputStream out) throws IOException { out.writeLong(result); }
        });
    }

    private static String metadataPattern(DatabaseMetaData meta, String name) throws SQLException {
        String escape = meta.getSearchStringEscape();
        if (escape == null || escape.isEmpty()) return name;
        return name.replace(escape, escape + escape).replace("%", escape + "%").replace("_", escape + "_");
    }

    private void describeTable(final long requestId, long sessionId, String schema, String table) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        final List<ColumnDescription> columns = new ArrayList<ColumnDescription>();
        final List<PrimaryColumn> keys = new ArrayList<PrimaryColumn>();
        try {
            DatabaseMetaData meta = holder.connection.getMetaData();
            if (schema == null || schema.isEmpty()) schema = holder.connection.getSchema();
            if (schema == null || schema.isEmpty()) throw new SQLException("cannot resolve validation schema");
            ResultSet rows = meta.getColumns(null, metadataPattern(meta, schema), metadataPattern(meta, table), null);
            try {
                while (rows.next()) {
                    if (!schema.equals(rows.getString("TABLE_SCHEM")) || !table.equals(rows.getString("TABLE_NAME"))) continue;
                    columns.add(new ColumnDescription(rows.getString("COLUMN_NAME"), rows.getString("TYPE_NAME"),
                            rows.getInt("DATA_TYPE"), rows.getLong("COLUMN_SIZE"), rows.getLong("DECIMAL_DIGITS"), rows.getInt("ORDINAL_POSITION")));
                }
            } finally { rows.close(); }
            rows = meta.getPrimaryKeys(null, schema, table);
            try { while (rows.next()) keys.add(new PrimaryColumn(rows.getString("COLUMN_NAME"), rows.getInt("KEY_SEQ"))); }
            finally { rows.close(); }
        } finally { s.pool.release(holder); }
        Collections.sort(columns, new Comparator<ColumnDescription>() { public int compare(ColumnDescription a, ColumnDescription b) { return Integer.compare(a.ordinal, b.ordinal); } });
        Collections.sort(keys, new Comparator<PrimaryColumn>() { public int compare(PrimaryColumn a, PrimaryColumn b) { return Integer.compare(a.ordinal, b.ordinal); } });
        if (columns.isEmpty()) throw new SQLException("validation table has no visible columns");
        writeSuccess(requestId, new BodyWriter() { public void write(DataOutputStream out) throws IOException {
            out.writeInt(columns.size());
            for (ColumnDescription c : columns) { writeString(out,c.name); writeString(out,c.type); out.writeInt(c.sqlType); out.writeLong(c.size); out.writeLong(c.scale); out.writeLong(c.size); }
            out.writeInt(keys.size()); for (PrimaryColumn key : keys) writeString(out,key.name);
        }});
    }

    private void openCursor(final long requestId, long sessionId, String sql) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        Statement statement = null;
        ResultSet rows = null;
        Cursor owned = null;
        try {
            statement = holder.connection.createStatement(ResultSet.TYPE_FORWARD_ONLY, ResultSet.CONCUR_READ_ONLY);
            statement.setFetchSize(128);
            activeStatements.put(requestId, statement);
            rows = statement.executeQuery(sql);
            if (Thread.currentThread().isInterrupted()) throw new SQLException("validation cursor cancelled");
            final long id = nextCursor.getAndIncrement();
            Cursor cursor = new Cursor(id, requestId, s, holder, statement, rows);
            owned = cursor;
            cursors.put(id,cursor);
            if (Thread.currentThread().isInterrupted()) throw new SQLException("validation cursor cancelled");
            writeSuccess(requestId, new BodyWriter() { public void write(DataOutputStream out) throws IOException { out.writeLong(id); }});
        } catch (Throwable error) {
            if (owned != null) closeCursor(owned.id);
            else { closeQuietly(rows); closeQuietly(statement); s.pool.discard(holder); }
            throw error;
        } finally { activeStatements.remove(requestId); }
    }

    private static byte[] readBoundedBytes(InputStream input, int limit) throws Exception {
        if (input == null) return null;
        try {
            ByteArrayOutputStream out = new ByteArrayOutputStream(); byte[] b = new byte[8192]; int n;
            while ((n=input.read(b)) >= 0) { if (out.size() + n > limit) throw new SQLException("validation binary value exceeds cursor byte limit"); out.write(b,0,n); }
            return out.toByteArray();
        } finally { input.close(); }
    }
    private static byte[] validationUTF8(String value) throws Exception {
        ByteBuffer encoded=StandardCharsets.UTF_8.newEncoder().onMalformedInput(CodingErrorAction.REPORT).onUnmappableCharacter(CodingErrorAction.REPORT).encode(CharBuffer.wrap(value));
        byte[] bytes=new byte[encoded.remaining()];encoded.get(bytes);return bytes;
    }
    private static void writeValidationText(DataOutputStream out,String value)throws Exception {
        byte[] bytes=validationUTF8(value);out.writeInt(bytes.length);out.write(bytes);
    }
    private static String readBoundedText(Reader input, int limit) throws Exception {
        if (input == null) return null;
        try {
            StringBuilder out = new StringBuilder(); char[] b = new char[4096]; int n;
            while ((n=input.read(b)) >= 0) { if (out.length() + n > limit) throw new SQLException("validation text value exceeds cursor byte limit"); out.append(b,0,n); }
            String value=out.toString(); if(validationUTF8(value).length > limit) throw new SQLException("validation text value exceeds cursor byte limit"); return value;
        } finally { input.close(); }
    }
    private static void writeCursorValue(DataOutputStream out, ResultSet rows, int column, int type, int limit) throws Exception {
        Object probe = rows.getObject(column);
        if (probe == null) { out.writeByte(NULL); return; }
        switch(type) {
            case Types.BOOLEAN: case Types.BIT: out.writeByte(BOOL); out.writeBoolean(rows.getBoolean(column)); return;
            case Types.TINYINT: case Types.SMALLINT: case Types.INTEGER: case Types.BIGINT: out.writeByte(INT64); out.writeLong(rows.getLong(column)); return;
            case Types.NUMERIC: case Types.DECIMAL: out.writeByte(DECIMAL); writeString(out,rows.getBigDecimal(column).toPlainString()); return;
            case Types.REAL: out.writeByte(FLOAT64); out.writeDouble((double)rows.getFloat(column)); return;
            case Types.FLOAT: case Types.DOUBLE: out.writeByte(FLOAT64); out.writeDouble(rows.getDouble(column)); return;
            case Types.BINARY: case Types.VARBINARY: case Types.LONGVARBINARY: case Types.BLOB: {
                byte[] value = readBoundedBytes(rows.getBinaryStream(column),limit); out.writeByte(BYTES); out.writeInt(value.length); out.write(value); return;
            }
            case Types.CHAR: case Types.NCHAR: case Types.VARCHAR: case Types.NVARCHAR: case Types.LONGVARCHAR: case Types.LONGNVARCHAR: case Types.CLOB: case Types.NCLOB: {
                out.writeByte(STRING); writeValidationText(out,readBoundedText(rows.getCharacterStream(column),limit)); return;
            }
            case Types.DATE: case Types.TIME: case Types.TIMESTAMP: case 2013: case 2014:
                out.writeByte(STRING); writeValidationText(out,rows.getString(column)); return;
            default: throw new SQLException("JDBC type " + type + " has no lossless validation serializer");
        }
    }
    private void fetchCursor(final long requestId, long id, int maxRows, int maxBytes) throws Exception {
        if (maxRows <= 0 || maxRows > 10000 || maxBytes <= 0 || maxBytes > MAX_FRAME-1024) throw new IOException("invalid cursor fetch limit");
        Cursor c = cursors.get(id);
        if (c == null) throw new SQLException("validation cursor is closed");
        synchronized(c) {
            activeStatements.put(requestId,c.statement); activeCursors.put(requestId,c);
            try {
                final List<byte[]> result = new ArrayList<byte[]>(); int size=5;
                while (result.size() < maxRows) {
                    byte[] row = c.pending;
                    c.pending = null;
                    if (row == null) {
                        if (!c.rows.next()) { c.done=true; break; }
                        ByteArrayOutputStream bytes=new ByteArrayOutputStream(); DataOutputStream out=new DataOutputStream(bytes);
                        int count=c.metadata.getColumnCount(); out.writeInt(count);
                        for(int i=1;i<=count;i++) { writeCursorValue(out,c.rows,i,c.metadata.getColumnType(i),maxBytes); if(bytes.size()+5>maxBytes) throw new SQLException("validation row exceeds cursor byte limit"); }
                        row=bytes.toByteArray();
                    }
                    if(size+row.length>maxBytes) { c.pending=row; break; }
                    result.add(row); size+=row.length;
                }
                final boolean done=c.done;
                if(done) closeCursor(id);
                writeSuccess(requestId,new BodyWriter(){public void write(DataOutputStream out)throws IOException{out.writeBoolean(done);out.writeInt(result.size());for(byte[] row:result)out.write(row);}});
            } catch(Throwable error) { closeCursor(id); throw error; }
            finally { activeStatements.remove(requestId); activeCursors.remove(requestId); }
        }
    }
    private void closeCursor(long id) {
        Cursor c=cursors.remove(id); if(c==null)return;
        synchronized(c) { try {c.statement.cancel();}catch(Throwable ignored){} closeQuietly(c.rows);closeQuietly(c.statement);c.session.pool.release(c.holder); }
    }

    private void beginBatch(final long requestId, long sessionId, String sql) throws Exception {
        Session s = session(sessionId);
        PooledConnection holder = s.pool.acquire();
        try {
            holder.connection.setAutoCommit(false);
            PreparedStatement statement = holder.connection.prepareStatement(sql);
            long id = nextBatch.getAndIncrement();
            batches.put(id, new Batch(id, s, holder, statement));
            final long batchId = id;
            writeSuccess(requestId, new BodyWriter() {
                @Override public void write(DataOutputStream out) throws IOException { out.writeLong(batchId); }
            });
        } catch (Throwable error) {
            rollbackQuietly(holder.connection);
            s.pool.discard(holder);
            throw error;
        }
    }

    private Batch batch(long id) throws SQLException {
        Batch result = batches.get(id);
        if (result == null) throw new SQLException("Oscar JDBC batch is closed", "25000");
        return result;
    }

    private void addBatch(long requestId, long batchId, DataInputStream body) throws Exception {
        Batch batch = batch(batchId);
        synchronized (batch) {
            activeStatements.put(requestId, batch.statement);
            try {
                int rowCount = body.readInt();
                for (int rowIndex = 0; rowIndex < rowCount; rowIndex++) {
                    int columns = body.readInt();
                    if (batch.parameterTypes == null) batch.loadParameterTypes(columns);
                    for (int column = 1; column <= columns; column++) {
                        bind(batch, body, column);
                    }
                    batch.statement.addBatch();
                }
                batch.statement.executeBatch();
                batch.statement.clearBatch();
            } finally {
                activeStatements.remove(requestId);
            }
        }
        writeSuccess(requestId, null);
    }

    private void bind(Batch batch, DataInputStream body, int index) throws Exception {
        int kind = body.readUnsignedByte();
        switch (kind) {
            case NULL:
                int sqlType = batch.parameterTypes != null && index <= batch.parameterTypes.length
                        ? batch.parameterTypes[index - 1] : java.sql.Types.NULL;
                if (sqlType == java.sql.Types.NULL || sqlType == 0) batch.statement.setObject(index, null);
                else batch.statement.setNull(index, sqlType);
                return;
            case BOOL:
                batch.statement.setBoolean(index, body.readUnsignedByte() != 0);
                return;
            case INT64:
                batch.statement.setLong(index, body.readLong());
                return;
            case UINT64:
            case DECIMAL:
                batch.statement.setBigDecimal(index, new BigDecimal(readString(body)));
                return;
            case FLOAT64:
                batch.statement.setDouble(index, body.readDouble());
                return;
            case STRING: {
                String value = readString(body);
                if (value.length() > 65535) batch.statement.setCharacterStream(index, new StringReader(value), value.length());
                else batch.statement.setString(index, value);
                return;
            }
            case BYTES: {
                byte[] value = readBytes(body);
                if (value.length > 65535) batch.statement.setBinaryStream(index, new ByteArrayInputStream(value), value.length);
                else batch.statement.setBytes(index, value);
                return;
            }
            case DATE:
                batch.statement.setDate(index, java.sql.Date.valueOf(readString(body)));
                return;
            case TIME:
                // Oscar accepts fractional seconds more reliably as text than java.sql.Time.
                batch.statement.setString(index, readString(body));
                return;
            case TIMESTAMP:
                batch.statement.setTimestamp(index, Timestamp.valueOf(readString(body)));
                return;
            default:
                throw new IOException("unsupported value kind " + kind);
        }
    }

    private void commitBatch(long requestId, long batchId) throws Exception {
        Batch batch = batch(batchId);
        synchronized (batch) {
            batches.remove(batchId);
            try {
                batch.holder.connection.commit();
                batch.holder.connection.setAutoCommit(true);
                closeQuietly(batch.statement);
                batch.session.pool.release(batch.holder);
            } catch (Throwable error) {
                rollbackQuietly(batch.holder.connection);
                closeQuietly(batch.statement);
                batch.session.pool.discard(batch.holder);
                throw error;
            }
        }
        writeSuccess(requestId, null);
    }

    private void abortBatch(long requestId, long batchId) throws Exception {
        Batch batch = batches.remove(batchId);
        if (batch != null) {
            synchronized (batch) {
                try { batch.statement.cancel(); } catch (Throwable ignored) { }
                rollbackQuietly(batch.holder.connection);
                closeQuietly(batch.statement);
                batch.session.pool.discard(batch.holder);
            }
        }
        writeSuccess(requestId, null);
    }

    private void closeSession(long requestId, long sessionId) throws Exception {
        Session session = sessions.remove(sessionId);
        if (session != null) {
            for (Cursor c : new ArrayList<Cursor>(cursors.values())) if(c.session==session) closeCursor(c.id);
            for (Map.Entry<Long, Batch> entry : new ArrayList<Map.Entry<Long, Batch>>(batches.entrySet())) {
                if (entry.getValue().session == session) {
                    Batch batch = batches.remove(entry.getKey());
                    if (batch != null) {
                        rollbackQuietly(batch.holder.connection);
                        closeQuietly(batch.statement);
                        session.pool.discard(batch.holder);
                    }
                }
            }
            session.pool.close();
        }
        writeSuccess(requestId, null);
    }

    private void cancelRequest(long target) {
        // Interrupt before attempting any JDBC cleanup. A driver may ignore
        // Statement.cancel while its fetch holds the cursor monitor.
        Future<?> future = requests.get(target);
        if (future != null) future.cancel(true);
        final Statement statement = activeStatements.get(target);
        Cursor active = activeCursors.get(target);
        final List<Cursor> toClose = new ArrayList<Cursor>();
        for (Cursor c : new ArrayList<Cursor>(cursors.values())) {
            if (c.openingRequest == target || c == active) toClose.add(c);
        }
        // Never wait on driver calls or cursor monitors on the protocol input
        // thread: other sessions and close requests must remain serviceable.
        if (statement != null || !toClose.isEmpty()) workers.execute(new Runnable() {
            @Override public void run() {
                if (statement != null) try { statement.cancel(); } catch (Throwable ignored) { }
                for (Cursor c : toClose) {
                    try { c.statement.cancel(); } catch (Throwable ignored) { }
                    closeCursor(c.id);
                }
            }
        });
    }

    private void closeEverything() {
        for(Cursor c:new ArrayList<Cursor>(cursors.values())) closeCursor(c.id);
        for (Future<?> request : requests.values()) request.cancel(true);
        for (Statement statement : activeStatements.values()) closeQuietly(statement);
        for (Batch batch : batches.values()) {
            rollbackQuietly(batch.holder.connection);
            closeQuietly(batch.statement);
            batch.session.pool.discard(batch.holder);
        }
        batches.clear();
        for (Session session : sessions.values()) session.pool.close();
        sessions.clear();
        workers.shutdownNow();
    }

    private interface BodyWriter { void write(DataOutputStream out) throws IOException; }

    private void writeSuccess(long requestId, BodyWriter writer) {
        try {
            ByteArrayOutputStream bytes = new ByteArrayOutputStream();
            DataOutputStream body = new DataOutputStream(bytes);
            body.writeByte(VERSION);
            body.writeByte(RESPONSE);
            body.writeLong(requestId);
            body.writeByte(0);
            if (writer != null) writer.write(body);
            writeFrame(bytes.toByteArray());
        } catch (IOException error) {
            System.err.println("Oscar bridge response failed: " + safeMessage(error));
        }
    }

    private void writeThrowable(long requestId, Throwable error) {
        SQLException sql = findSQLException(error);
        String state = sql == null || sql.getSQLState() == null ? "" : sql.getSQLState();
        int code = sql == null ? 0 : sql.getErrorCode();
        String message = safeMessage(error);
        if (error instanceof BatchUpdateException) {
            message += " (successful batch entries: " + ((BatchUpdateException) error).getUpdateCounts().length + ")";
        }
        writeError(requestId, state, code, message);
    }

    private void writeError(long requestId, String state, int code, String message) {
        try {
            ByteArrayOutputStream bytes = new ByteArrayOutputStream();
            DataOutputStream body = new DataOutputStream(bytes);
            body.writeByte(VERSION);
            body.writeByte(RESPONSE);
            body.writeLong(requestId);
            body.writeByte(1);
            writeString(body, state);
            body.writeInt(code);
            writeString(body, message);
            writeFrame(bytes.toByteArray());
        } catch (IOException error) {
            System.err.println("Oscar bridge error response failed: " + safeMessage(error));
        }
    }

    private void writeFrame(byte[] frame) throws IOException {
        synchronized (outputLock) {
            output.writeInt(frame.length);
            output.write(frame);
            output.flush();
        }
    }

    private static SQLException findSQLException(Throwable error) {
        Throwable current = error;
        while (current != null) {
            if (current instanceof SQLException) return (SQLException) current;
            current = current.getCause();
        }
        return null;
    }

    private static String safeMessage(Throwable error) {
        String message = error.getMessage();
        return message == null || message.trim().isEmpty() ? error.getClass().getSimpleName() : message;
    }

	private static String driverVersion() {
		try {
			Class<?> driver = Class.forName("com.oscar.Driver");
			Object value = driver.getMethod("getVersion").invoke(null);
			return value == null ? "unknown" : value.toString();
		} catch (Throwable ignored) {
			return "unknown";
		}
	}

    private static String readString(DataInputStream in) throws IOException {
        int length = in.readInt();
        if (length < 0 || length > MAX_FRAME) throw new IOException("invalid string length " + length);
        byte[] value = new byte[length];
        in.readFully(value);
        return new String(value, StandardCharsets.UTF_8);
    }

    private static byte[] readBytes(DataInputStream in) throws IOException {
        int length = in.readInt();
        if (length < 0 || length > MAX_FRAME) throw new IOException("invalid byte array length " + length);
        byte[] value = new byte[length];
        in.readFully(value);
        return value;
    }

    private static void writeString(DataOutputStream out, String value) throws IOException {
        byte[] bytes = value.getBytes(StandardCharsets.UTF_8);
        out.writeInt(bytes.length);
        out.write(bytes);
    }

    private static void closeQuietly(AutoCloseable value) {
        if (value != null) try { value.close(); } catch (Throwable ignored) { }
    }

    private static void rollbackQuietly(Connection connection) {
        if (connection != null) try { connection.rollback(); } catch (Throwable ignored) { }
    }

    private static final class Session {
        final ConnectionPool pool;
        Session(String url, String username, String password, String schema, int maxOpen,
                int maxIdle, long lifetimeMs, int loginTimeoutMs, int socketTimeoutMs) {
            this.pool = new ConnectionPool(url, username, password, schema, maxOpen, maxIdle,
                    lifetimeMs, loginTimeoutMs, socketTimeoutMs);
        }
    }

    private static final class ColumnDescription {
        final String name,type; final int sqlType,ordinal; final long size,scale;
        ColumnDescription(String name,String type,int sqlType,long size,long scale,int ordinal){this.name=name;this.type=type;this.sqlType=sqlType;this.size=size;this.scale=scale;this.ordinal=ordinal;}
    }
    private static final class PrimaryColumn { final String name; final int ordinal; PrimaryColumn(String name,int ordinal){this.name=name;this.ordinal=ordinal;} }
    private static final class Cursor {
        final long id,openingRequest; final Session session; final PooledConnection holder; final Statement statement; final ResultSet rows; final ResultSetMetaData metadata; byte[] pending; boolean done;
        Cursor(long id,long openingRequest,Session session,PooledConnection holder,Statement statement,ResultSet rows)throws SQLException{this.id=id;this.openingRequest=openingRequest;this.session=session;this.holder=holder;this.statement=statement;this.rows=rows;this.metadata=rows.getMetaData();}
    }

    private static final class Batch {
        final long id;
        final Session session;
        final PooledConnection holder;
        final PreparedStatement statement;
        int[] parameterTypes;

        Batch(long id, Session session, PooledConnection holder, PreparedStatement statement) {
            this.id = id;
            this.session = session;
            this.holder = holder;
            this.statement = statement;
        }

        void loadParameterTypes(int columns) {
            parameterTypes = new int[columns];
            try {
                ParameterMetaData metadata = statement.getParameterMetaData();
                for (int i = 0; i < columns; i++) parameterTypes[i] = metadata.getParameterType(i + 1);
            } catch (Throwable ignored) {
                parameterTypes = new int[columns];
            }
        }
    }

    private static final class PooledConnection {
        final Connection connection;
        final long createdAt;
        PooledConnection(Connection connection) {
            this.connection = connection;
            this.createdAt = System.currentTimeMillis();
        }
    }

    private static final class ConnectionPool {
        final String url;
        final String username;
        final String password;
        final String schema;
        final int maxOpen;
        final int maxIdle;
        final long lifetimeMs;
        final int loginTimeoutMs;
        final int socketTimeoutMs;
        final ArrayDeque<PooledConnection> idle = new ArrayDeque<PooledConnection>();
        int open;
        boolean closed;

        ConnectionPool(String url, String username, String password, String schema, int maxOpen,
                int maxIdle, long lifetimeMs, int loginTimeoutMs, int socketTimeoutMs) {
            this.url = url;
            this.username = username;
            this.password = password;
            this.schema = schema;
            this.maxOpen = Math.max(1, maxOpen);
            this.maxIdle = Math.max(0, Math.min(maxIdle, this.maxOpen));
            this.lifetimeMs = Math.max(0, lifetimeMs);
            this.loginTimeoutMs = Math.max(0, loginTimeoutMs);
            this.socketTimeoutMs = Math.max(0, socketTimeoutMs);
        }

        void test() throws Exception {
            PooledConnection holder = acquire();
            try {
                if (!holder.connection.isValid(Math.max(1, loginTimeoutMs / 1000))) {
                    throw new SQLException("Oscar JDBC connection validation failed", "08006");
                }
            } finally {
                release(holder);
            }
        }

        synchronized PooledConnection acquire() throws Exception {
            while (true) {
                if (closed) throw new SQLException("Oscar JDBC connection pool is closed", "08003");
                while (!idle.isEmpty()) {
                    PooledConnection holder = idle.pop();
                    if (expired(holder) || holder.connection.isClosed()) {
                        closeQuietly(holder.connection);
                        open--;
                        continue;
                    }
                    return holder;
                }
                if (open < maxOpen) {
                    open++;
                    try {
                        return create();
                    } catch (Throwable error) {
                        open--;
                        notifyAll();
                        throw error;
                    }
                }
                wait(1000L);
            }
        }

        private PooledConnection create() throws Exception {
            Properties props = new Properties();
            props.setProperty("user", username);
            props.setProperty("password", password);
            props.setProperty("LOGINTIMEOUT", Integer.toString(loginTimeoutMs));
            props.setProperty("SOCKETTIMEOUT", Integer.toString(socketTimeoutMs));
            props.setProperty("tcpKeepAlive", "true");
            props.setProperty("loglevel", "0");
            Connection connection = DriverManager.getConnection(url, props);
            if (schema != null && !schema.isEmpty()) connection.setSchema(schema);
            return new PooledConnection(connection);
        }

        synchronized void release(PooledConnection holder) {
            if (holder == null) return;
            if (closed || expired(holder) || idle.size() >= maxIdle) {
                closeQuietly(holder.connection);
                open--;
            } else {
                idle.push(holder);
            }
            notifyAll();
        }

        synchronized void discard(PooledConnection holder) {
            if (holder != null) {
                closeQuietly(holder.connection);
                open--;
                notifyAll();
            }
        }

        synchronized void close() {
            closed = true;
            while (!idle.isEmpty()) {
                closeQuietly(idle.pop().connection);
                open--;
            }
            notifyAll();
        }

        private boolean expired(PooledConnection holder) {
            return lifetimeMs > 0 && System.currentTimeMillis() - holder.createdAt >= lifetimeMs;
        }
    }
}
