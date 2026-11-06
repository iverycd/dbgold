package com.dbgold.oscar;

import java.io.*;
import java.lang.reflect.*;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.security.MessageDigest;
import java.sql.*;
import java.util.*;
import java.util.concurrent.*;

/** Real files and streams, fake JDBC: exercises LOBs without a vendor database. */
public final class BridgeMainLOBTest {
    private static final Class<?> MAIN = BridgeMain.class;
    private static final int CHUNK = 1024 * 1024;

    private static Class<?> nested(String name) throws Exception {
        return Class.forName("com.dbgold.oscar.BridgeMain$" + name);
    }
    private static Object construct(Class<?> type, Class<?>[] parameters, Object... values) throws Exception {
        Constructor<?> constructor = type.getDeclaredConstructor(parameters);
        constructor.setAccessible(true);
        return constructor.newInstance(values);
    }
    private static Object get(Object target, String name) throws Exception {
        Field field = target.getClass().getDeclaredField(name);
        field.setAccessible(true);
        return field.get(target);
    }
    private static void set(Object target, String name, Object value) throws Exception {
        Field field = target.getClass().getDeclaredField(name);
        field.setAccessible(true);
        field.set(target, value);
    }
    private static Object call(Object target, String name, Class<?>[] parameters, Object... values) throws Exception {
        Method method = target.getClass().getDeclaredMethod(name, parameters);
        method.setAccessible(true);
        try { return method.invoke(target, values); }
        catch (InvocationTargetException error) {
            Throwable cause = error.getCause();
            if (cause instanceof Exception) throw (Exception) cause;
            throw error;
        }
    }
    private static void check(boolean success, String message) {
        if (!success) throw new AssertionError(message);
    }
    private interface Body { void write(DataOutputStream out) throws Exception; }
    private static DataInputStream body(Body writer) throws Exception {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        writer.write(new DataOutputStream(bytes));
        return new DataInputStream(new ByteArrayInputStream(bytes.toByteArray()));
    }
    private interface Action { void run() throws Exception; }
    private static void rejects(Action action, String message) throws Exception {
        try { action.run(); } catch (IOException expected) { return; } catch (SQLException expected) { return; }
        throw new AssertionError(message);
    }

    private static final class Bound {
        final Object stream;
        final long length;
        Bound(Object stream, long length) { this.stream = stream; this.length = length; }
    }

    private static final class Fixture implements AutoCloseable {
        final Object bridge, session, pool;
        final Path directory;
        final Map<Long, Object> batches;
        final List<Bound> bound = new ArrayList<Bound>();
        final List<byte[]> digests = new ArrayList<byte[]>();
        final List<Long> lengths = new ArrayList<Long>();
        final List<Bound> executed = new ArrayList<Bound>();
        final CountDownLatch entered = new CountDownLatch(1);
        final CountDownLatch release = new CountDownLatch(1);
        final CountDownLatch prepareEntered = new CountDownLatch(1);
        final CountDownLatch prepareRelease = new CountDownLatch(1);
        final CountDownLatch statementCancelled = new CountDownLatch(1);
        final PreparedStatement statement;
        final Object holder;
        volatile boolean failExecute, blockExecute, blockUntilCancel, blockPrepare;
        volatile int commits, rollbacks, executes, connectionCloses;

        @SuppressWarnings("unchecked") Fixture() throws Exception {
            directory = Files.createTempDirectory("dbgold-lob-java-test-");
            System.setProperty("dbgold.oscar.spoolDir", directory.toString());
            bridge = construct(MAIN, new Class<?>[]{});
            set(bridge, "output", new DataOutputStream(new ByteArrayOutputStream()));
            session = construct(nested("Session"), new Class<?>[]{String.class, String.class, String.class, String.class,
                    int.class, int.class, long.class, int.class, int.class}, "url", "user", "password", "schema", 1, 1, 0L, 0, 0);
            pool = get(session, "pool");
            set(pool, "open", 1);
            ((Map<Long, Object>) get(bridge, "sessions")).put(1L, session);
            Connection connection = (Connection) Proxy.newProxyInstance(Connection.class.getClassLoader(), new Class<?>[]{Connection.class},
                    new InvocationHandler() {
                        public Object invoke(Object p, Method m, Object[] args) throws Exception {
                            String name = m.getName();
                            if (name.equals("commit")) commits++;
                            if (name.equals("rollback")) rollbacks++;
                            if (name.equals("close")) connectionCloses++;
                            if (name.equals("isClosed")) return false;
                            if (name.equals("prepareStatement")) {
                                prepareEntered.countDown();
                                if (blockPrepare) prepareRelease.await();
                                return Fixture.this.statement;
                            }
                            return null;
                        }
                    });
            holder = construct(nested("PooledConnection"), new Class<?>[]{Connection.class}, connection);
            statement = (PreparedStatement) Proxy.newProxyInstance(PreparedStatement.class.getClassLoader(), new Class<?>[]{PreparedStatement.class},
                    new InvocationHandler() {
                        public Object invoke(Object p, Method m, Object[] args) throws Exception {
                            String name = m.getName();
                            if (name.equals("cancel")) statementCancelled.countDown();
                            if (name.equals("setCharacterStream") || name.equals("setBinaryStream")) {
                                check(args.length == 3 && args[2] instanceof Long, "LOB setter must use exact long length");
                                bound.add(new Bound(args[1], (Long) args[2]));
                            }
                            if (name.equals("executeBatch")) {
                                executes++;
                                entered.countDown();
                                if (blockExecute) release.await();
                                if (blockUntilCancel) {
                                    boolean cancelled = false;
                                    while (!cancelled) {
                                        try { statementCancelled.await(); cancelled = true; }
                                        catch (InterruptedException ignored) { }
                                    }
                                }
                                if (failExecute) throw new SQLException("injected batch execution failure");
                                for (Bound value : bound) {
                                    MessageDigest hash = MessageDigest.getInstance("SHA-256");
                                    long length = 0;
                                    if (value.stream instanceof Reader) {
                                        char[] buffer = new char[8192]; int count;
                                        while ((count = ((Reader) value.stream).read(buffer)) >= 0) {
                                            for (int i = 0; i < count; i++) {
                                                hash.update((byte) (buffer[i] >>> 8)); hash.update((byte) buffer[i]);
                                            }
                                            length += count;
                                        }
                                    } else {
                                        byte[] buffer = new byte[8192]; int count;
                                        while ((count = ((InputStream) value.stream).read(buffer)) >= 0) {
                                            hash.update(buffer, 0, count); length += count;
                                        }
                                    }
                                    check(length == value.length, "JDBC character/byte length differs from actual stream");
                                    digests.add(hash.digest()); lengths.add(length); executed.add(value);
                                }
                                bound.clear();
                                return new int[]{1};
                            }
                            if (name.equals("clearBatch") || name.equals("close")) bound.clear();
                            return null;
                        }
                    });
            batches = (Map<Long, Object>) get(bridge, "batches");
        }

        void batch(long id) throws Exception {
            Object batch = construct(nested("Batch"), new Class<?>[]{long.class, nested("Session"), nested("PooledConnection"), PreparedStatement.class, long.class},
                    id, session, holder, statement, 10000L + id);
            batches.put(id, batch);
        }
        void begin(final long batch, final long id, final int kind, final long length) throws Exception {
            call(bridge, "beginLOB", new Class<?>[]{long.class, long.class, DataInputStream.class}, 1L, batch,
                    body(new Body() { public void write(DataOutputStream out) throws Exception { out.writeLong(id); out.writeByte(kind); out.writeLong(length); } }));
        }
        void chunk(final long batch, final long id, final long offset, final byte[] bytes, final int size) throws Exception {
            call(bridge, "appendLOB", new Class<?>[]{long.class, long.class, DataInputStream.class}, 2L, batch,
                    body(new Body() { public void write(DataOutputStream out) throws Exception { out.writeLong(id); out.writeLong(offset); out.writeInt(size); out.write(bytes, 0, size); } }));
        }
        void finish(long batch, long id) throws Exception {
            call(bridge, "finishLOB", new Class<?>[]{long.class, long.class, long.class}, 3L, batch, id);
        }
        void upload(long batch, long id, int kind, byte[] bytes) throws Exception {
            begin(batch, id, kind, bytes.length);
            for (int offset = 0; offset < bytes.length; offset += CHUNK) {
                int size = Math.min(CHUNK, bytes.length - offset);
                chunk(batch, id, offset, Arrays.copyOfRange(bytes, offset, offset + size), size);
            }
            finish(batch, id);
        }
        byte[] uploadLarge(long batch, long id, int kind, long size) throws Exception {
            begin(batch, id, kind, size);
            MessageDigest hash = MessageDigest.getInstance("SHA-256");
            byte[] bytes = new byte[CHUNK];
            for (long offset = 0; offset < size; offset += CHUNK) {
                int count = (int) Math.min(CHUNK, size - offset);
                for (int i = 0; i < count; i++) bytes[i] = kind == 1 ? (byte) 'x' : (byte) ((offset + i) % 251);
                chunk(batch, id, offset, bytes, count);
                if (kind == 1) {
                    byte[] utf16 = new byte[count * 2];
                    for (int i = 0; i < count; i++) utf16[i * 2 + 1] = 'x';
                    hash.update(utf16);
                } else hash.update(bytes, 0, count);
            }
            finish(batch, id);
            return hash.digest();
        }
        void add(final long request, final long batch, final long... ids) throws Exception {
            call(bridge, "addBatch", new Class<?>[]{long.class, long.class, DataInputStream.class}, request, batch,
                    body(new Body() { public void write(DataOutputStream out) throws Exception {
                        out.writeInt(1); out.writeInt(ids.length);
                        for (long id : ids) { out.writeByte(11); out.writeLong(id); }
                    } }));
        }
        void abort(long id) throws Exception { call(bridge, "abortBatch", new Class<?>[]{long.class, long.class}, 5L, id); }
        void commit(long id) throws Exception { call(bridge, "commitBatch", new Class<?>[]{long.class, long.class}, 6L, id); }
        boolean filesEmpty() throws Exception {
            if (!Files.exists(directory)) return true;
            try (java.util.stream.Stream<Path> paths = Files.list(directory)) { return !paths.findAny().isPresent(); }
        }
        public void close() throws Exception {
            call(bridge, "closeEverything", new Class<?>[]{});
            check(filesEmpty(), "temporary LOB files leaked after close");
            Files.deleteIfExists(directory);
        }
    }

    private static byte[] textDigest(String text) throws Exception {
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        for (int i = 0; i < text.length(); i++) {
            digest.update((byte) (text.charAt(i) >>> 8)); digest.update((byte) text.charAt(i));
        }
        return digest.digest();
    }

    private static void largeAndUnicodeRoundtrip() throws Exception {
        try (Fixture f = new Fixture()) {
            f.batch(1);
            byte[] textHash = f.uploadLarge(1, 11, 1, 91280898L);
            byte[] binaryHash = f.uploadLarge(1, 12, 2, 91280898L);
            f.add(4, 1, 11, 12);
            check(Arrays.equals(textHash, f.digests.get(0)), "91 MB text content changed");
            check(Arrays.equals(binaryHash, f.digests.get(1)), "91 MB binary content changed");
            check(f.commits == 0 && f.filesEmpty(), "AddBatch committed or retained consumed files");
            for (final Bound bound : f.executed) rejects(new Action() { public void run() throws Exception {
                if (bound.stream instanceof Reader) ((Reader) bound.stream).read(); else ((InputStream) bound.stream).read();
            } }, "JDBC stream was not closed after executeBatch");

            char[] prefix = new char[CHUNK - 1]; Arrays.fill(prefix, 'a');
            String unicode = new String(prefix) + "中😀\u0000尾";
            f.upload(1, 13, 1, unicode.getBytes(StandardCharsets.UTF_8));
            f.add(7, 1, 13);
            check(Arrays.equals(textDigest(unicode), f.digests.get(2)), "UTF-8 split inside a code point changed text");
            check(f.lengths.get(2) == unicode.length(), "supplementary characters counted as bytes");
            f.upload(1, 14, 1, new byte[0]); f.upload(1, 15, 2, new byte[0]); f.add(8, 1, 14, 15);
            check(f.lengths.get(3) == 0L && f.lengths.get(4) == 0L, "empty LOB changed");
            f.commit(1);
            check(f.commits == 1 && f.rollbacks == 0, "large row did not use one final commit");
        }
    }

    private static void invalidUploadsAndReferences() throws Exception {
        try (final Fixture f = new Fixture()) {
            f.batch(1); f.begin(1, 1, 1, 3);
            rejects(new Action() { public void run() throws Exception { f.chunk(1, 1, 1, new byte[]{1}, 1); } }, "wrong offset accepted");
            f.abort(1); check(f.filesEmpty(), "failed upload file leaked");
            f.batch(2); f.begin(2, 2, 2, 3);
            rejects(new Action() { public void run() throws Exception { f.finish(2, 2); } }, "incomplete LOB sealed"); f.abort(2);
            f.batch(3); f.begin(3, 3, 2, 1);
            rejects(new Action() { public void run() throws Exception { f.add(4, 3, 3); } }, "unsealed LOB referenced"); f.abort(3);
            f.batch(4); f.upload(4, 4, 1, new byte[]{'x'}); f.batch(5);
            rejects(new Action() { public void run() throws Exception { f.add(4, 5, 4); } }, "cross-batch LOB reference accepted"); f.abort(4); f.abort(5);
            f.batch(6); f.upload(6, 6, 2, new byte[]{0, 1, 2});
            rejects(new Action() { public void run() throws Exception { f.add(4, 6, 6, 6); } }, "duplicate LOB reference accepted"); f.abort(6);
            f.batch(7); f.begin(7, 7, 1, 2); f.chunk(7, 7, 0, new byte[]{(byte) 0xc3, 0x28}, 2);
            rejects(new Action() { public void run() throws Exception { f.finish(7, 7); } }, "malformed UTF-8 silently replaced"); f.abort(7);
            f.batch(8); f.begin(8, 8, 2, 1);
            rejects(new Action() { public void run() throws Exception { f.commit(8); } }, "pending LOB committed");
            f.batch(9); f.upload(9, 9, 2, new byte[]{0}); f.add(4, 9, 9);
            rejects(new Action() { public void run() throws Exception { f.begin(9, 9, 2, 1); } }, "consumed LOB id reused"); f.abort(9);
            check(f.commits == 0 && f.filesEmpty(), "invalid batches committed or retained files");
        }
    }

    @SuppressWarnings("unchecked") private static void closeDuringBatchOpening() throws Exception {
        for (final boolean everything : new boolean[]{false, true}) {
            try (final Fixture f = new Fixture()) {
                ((ArrayDeque<Object>) get(f.pool, "idle")).push(f.holder);
                f.blockPrepare = true;
                FutureTask<Void> opening = new FutureTask<Void>(new Callable<Void>() { public Void call() throws Exception {
                    BridgeMainLOBTest.call(f.bridge, "beginBatch", new Class<?>[]{long.class, long.class, String.class}, 70L, 1L, "INSERT INTO T VALUES (?)");
                    return null;
                } });
                Thread worker = new Thread(opening); worker.start();
                check(f.prepareEntered.await(2, TimeUnit.SECONDS), "prepareStatement did not start");
                if (everything) call(f.bridge, "closeEverything", new Class<?>[]{});
                else call(f.bridge, "closeSession", new Class<?>[]{long.class, long.class}, 8L, 1L);
                f.prepareRelease.countDown();
                worker.join(2000); check(!worker.isAlive(), "batch opening did not finish after session close");
                try { opening.get(); throw new AssertionError("batch created after session/global close"); }
                catch (ExecutionException expected) { check(expected.getCause() instanceof SQLException, "wrong opening failure"); }
                check(f.batches.isEmpty() && f.filesEmpty(), "closing race retained a batch or files");
                check((Integer) get(f.pool, "open") == 0 && f.connectionCloses == 1, "closing race leaked or discarded connection twice");
            }
        }
    }

    private static void explicitCloseCancelsBeforeWaitingForBatch() throws Exception {
        for (final boolean sessionClose : new boolean[]{false, true}) {
            try (final Fixture f = new Fixture()) {
                f.batch(1); f.upload(1, 1, 1, new byte[]{'x'}); f.blockUntilCancel = true;
                FutureTask<Void> execution = new FutureTask<Void>(new Callable<Void>() { public Void call() throws Exception { f.add(99L, 1L, 1L); return null; } });
                Thread worker = new Thread(execution); worker.start();
                check(f.entered.await(2, TimeUnit.SECONDS), "blocking execute did not start");
                FutureTask<Void> closing = new FutureTask<Void>(new Callable<Void>() { public Void call() throws Exception {
                    if (sessionClose) BridgeMainLOBTest.call(f.bridge, "closeSession", new Class<?>[]{long.class, long.class}, 8L, 1L);
                    else f.abort(1);
                    return null;
                } });
                Thread closer = new Thread(closing); closer.start();
                try { closing.get(2, TimeUnit.SECONDS); }
                finally { f.statementCancelled.countDown(); }
                worker.join(2000); check(!worker.isAlive(), "explicit close waited before signaling Statement.cancel");
                check(f.batches.isEmpty() && f.filesEmpty() && f.rollbacks == 1 && f.commits == 0,
                        "explicit close did not roll back and clean the batch");
            }
        }
    }

    private static void laterFailureRollsBackAndCloseCleansUploads() throws Exception {
        try (final Fixture f = new Fixture()) {
            f.batch(1); f.upload(1, 1, 1, new byte[]{'x'}); f.add(4, 1, 1);
            f.upload(1, 2, 2, new byte[]{0, 1, 2}); f.failExecute = true;
            rejects(new Action() { public void run() throws Exception { f.add(8, 1, 2); } }, "injected batch failure succeeded");
            check(f.filesEmpty(), "failed execute retained consumed files");
            f.abort(1); check(f.commits == 0 && f.rollbacks == 1, "later failure failed to roll back whole page");
            f.batch(2); f.begin(2, 3, 2, 100);
            call(f.bridge, "closeSession", new Class<?>[]{long.class, long.class}, 9L, 1L);
            check(f.filesEmpty(), "session close retained partial upload");
        }
    }

    @SuppressWarnings("unchecked") private static void cancellationClosesBatchBeforeCleanup() throws Exception {
        try (final Fixture f = new Fixture()) {
            f.batch(1); f.upload(1, 1, 1, new byte[]{'x'}); f.blockExecute = true;
            FutureTask<Void> task = new FutureTask<Void>(new Callable<Void>() { public Void call() throws Exception { f.add(99, 1, 1); return null; } });
            ((Map<Long, Future<?>>) get(f.bridge, "requests")).put(99L, task);
            Thread worker = new Thread(task); worker.start();
            check(f.entered.await(2, TimeUnit.SECONDS), "execute did not start");
            long started = System.nanoTime();
            call(f.bridge, "cancelRequest", new Class<?>[]{long.class}, 99L);
            check(System.nanoTime() - started < TimeUnit.MILLISECONDS.toNanos(500), "cancel blocked protocol input thread");
            worker.join(2000); check(!worker.isAlive(), "cancel did not interrupt batch execution");
            for (int i = 0; i < 100 && (!f.batches.isEmpty() || !f.filesEmpty()); i++) Thread.sleep(10);
            check(f.batches.isEmpty() && f.filesEmpty(), "cancel retained batch or LOB files");
            check(f.commits == 0 && f.rollbacks == 1, "cancel failed to roll back page");
            f.abort(1); check(f.rollbacks == 1, "repeated abort rolled back/discarded connection twice");
        }
    }

    public static void main(String[] args) throws Exception {
        largeAndUnicodeRoundtrip();
        invalidUploadsAndReferences();
        laterFailureRollsBackAndCloseCleansUploads();
        cancellationClosesBatchBeforeCleanup();
        closeDuringBatchOpening();
        explicitCloseCancelsBeforeWaitingForBatch();
    }
}
