package com.dbgold.oscar;

import java.io.*;
import java.lang.reflect.*;
import java.math.BigDecimal;
import java.sql.*;
import java.util.Map;
import java.util.ArrayDeque;
import java.util.Set;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Future;
import java.util.concurrent.FutureTask;
import java.util.concurrent.TimeUnit;

/** Dependency-free protocol checks using fake JDBC objects; no vendor database. */
public final class BridgeMainValidationTest {
    private static int closed;
    private static Object proxy(Class<?> kind, InvocationHandler handler) {
        return Proxy.newProxyInstance(kind.getClassLoader(),new Class<?>[]{kind},handler);
    }
    private static ResultSet rows(final String[] values) {
        return rows(values, Types.VARCHAR);
    }
    private static ResultSet rows(final String[] values, final int type) {
        final int[] pos={-1};
        final ResultSetMetaData meta=(ResultSetMetaData)proxy(ResultSetMetaData.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[] a){
            if(m.getName().equals("getColumnCount"))return 1;
            if(m.getName().equals("getColumnType"))return type;
            return null;
        }});
        return (ResultSet)proxy(ResultSet.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[] a){
            String n=m.getName();
            if(n.equals("next"))return ++pos[0]<values.length;
            if(n.equals("getMetaData"))return meta;
            if(n.equals("getObject")||n.equals("getString"))return values[pos[0]];
            if(n.equals("wasNull"))return values[pos[0]] == null;
            if(n.equals("getCharacterStream"))return new StringReader(values[pos[0]]);
            if(n.equals("getBigDecimal"))return new BigDecimal(values[pos[0]]);
            if(n.equals("close")){closed++;return null;}
            return null;
        }});
    }
    private static ResultSet catalogRows(final boolean keys) {
        final int[] pos={-1};
        return (ResultSet)proxy(ResultSet.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[] a){
            String n=m.getName(); if(n.equals("next"))return ++pos[0]==0; if(n.equals("close"))return null;
            if(n.equals("wasNull"))return false;
            String key=(String)a[0];
            if(n.equals("getString")){if(key.equals("TABLE_SCHEM"))return "s";if(key.equals("TABLE_NAME"))return "T";if(key.equals("TYPE_NAME"))return "DECIMAL";return "MiXeD";}
            if(n.equals("getInt"))return key.equals("DATA_TYPE")?Types.DECIMAL:1;
            if(n.equals("getLong"))return key.equals("DECIMAL_DIGITS")?19L:38L;
            return null;
        }});
    }
    private static Class<?> nested(String name)throws Exception{return Class.forName("com.dbgold.oscar.BridgeMain$"+name);}
    private static Object construct(Class<?> type,Class<?>[] params,Object...args)throws Exception{Constructor<?> c=type.getDeclaredConstructor(params);c.setAccessible(true);return c.newInstance(args);}
    private static Method method(Class<?> type,String name,Class<?>...params)throws Exception{Method m=type.getDeclaredMethod(name,params);m.setAccessible(true);return m;}
    private static void check(boolean ok,String message){if(!ok)throw new AssertionError(message);}
    @SuppressWarnings("unchecked") public static void main(String[]args)throws Exception{
        Class<?> main=Class.forName("com.dbgold.oscar.BridgeMain");
        Method write=method(main,"writeCursorValue",DataOutputStream.class,ResultSet.class,int.class,int.class,int.class);
        ResultSet decimal=rows(new String[]{"12345678901234567890.12345678901234567890"});decimal.next();
        ByteArrayOutputStream payload=new ByteArrayOutputStream();write.invoke(null,new DataOutputStream(payload),decimal,1,Types.DECIMAL,1024);
        DataInputStream dec=new DataInputStream(new ByteArrayInputStream(payload.toByteArray()));check(dec.readByte()==5,"decimal tag");int size=dec.readInt();byte[] exact=new byte[size];dec.readFully(exact);check(new String(exact,"UTF-8").equals("12345678901234567890.12345678901234567890"),"decimal precision");
        ResultSet text=rows(new String[]{"a\u0000b "});text.next();payload.reset();write.invoke(null,new DataOutputStream(payload),text,1,Types.VARCHAR,1024);check(payload.toByteArray()[6]==0,"NUL preserved");
        ResultSet malformed=rows(new String[]{"\ud800"});malformed.next();boolean invalidUnicode=false;
        try{write.invoke(null,new DataOutputStream(new ByteArrayOutputStream()),malformed,1,Types.VARCHAR,1024);}catch(InvocationTargetException e){invalidUnicode=e.getCause() instanceof java.nio.charset.CharacterCodingException;}
        check(invalidUnicode,"malformed UTF-16 cannot silently become replacement bytes");
        Object bridge=construct(main,new Class<?>[]{});
        ByteArrayOutputStream frames=new ByteArrayOutputStream();Field output=main.getDeclaredField("output");output.setAccessible(true);output.set(bridge,new DataOutputStream(frames));
        Class<?> sessionType=nested("Session"),holderType=nested("PooledConnection"),cursorType=nested("Cursor");
        Object session=construct(sessionType,new Class<?>[]{String.class,String.class,String.class,String.class,int.class,int.class,long.class,int.class,int.class},"url","u","p","s",1,1,0L,0,0);
        final ResultSet[] queryResult={null};
        final Statement statement=(Statement)proxy(Statement.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[]a){if(m.getName().equals("executeQuery"))return queryResult[0];return null;}});
        final DatabaseMetaData catalog=(DatabaseMetaData)proxy(DatabaseMetaData.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[]a){
            if(m.getName().equals("getSearchStringEscape"))return "\\";
            if(m.getName().equals("getColumns"))return catalogRows(false);
            if(m.getName().equals("getPrimaryKeys"))return catalogRows(true);
            return null;
        }});
        Connection conn=(Connection)proxy(Connection.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[]a){if(m.getName().equals("isClosed"))return false;if(m.getName().equals("createStatement"))return statement;if(m.getName().equals("getMetaData"))return catalog;if(m.getName().equals("getSchema"))return "s";return null;}});
        Object holder=construct(holderType,new Class<?>[]{Connection.class},conn);
        Field sessionsField=main.getDeclaredField("sessions");sessionsField.setAccessible(true);((Map<Long,Object>)sessionsField.get(bridge)).put(9L,session);
        Field poolField=sessionType.getDeclaredField("pool");poolField.setAccessible(true);Object pool=poolField.get(session);Field idleField=pool.getClass().getDeclaredField("idle");idleField.setAccessible(true);ArrayDeque<Object> idle=(ArrayDeque<Object>)idleField.get(pool);idle.push(holder);Field openField=pool.getClass().getDeclaredField("open");openField.setAccessible(true);openField.setInt(pool,1);
        method(main,"describeTable",long.class,long.class,String.class,String.class).invoke(bridge,9L,9L,"s","T");
        DataInputStream description=new DataInputStream(new ByteArrayInputStream(frames.toByteArray()));description.readInt();description.skipBytes(11);check(description.readInt()==1,"describe visible columns");int nameLength=description.readInt();byte[] name=new byte[nameLength];description.readFully(name);check(new String(name,"UTF-8").equals("MiXeD"),"metadata original case");frames.reset();
        Field cursorsField=main.getDeclaredField("cursors");cursorsField.setAccessible(true);Map<Long,Object> cursors=(Map<Long,Object>)cursorsField.get(bridge);
        String value=new String(new char[100]).replace('\0','x');
        queryResult[0]=rows(new String[]{value,value});
        Method open=method(main,"openCursor",long.class,long.class,String.class,String[].class,Set.class);
        open.invoke(bridge,42L,9L,"SELECT MiXeD FROM s.T",null,null);check(cursors.size()==1,"open cursor retained");frames.reset();
        Method fetch=method(main,"fetchCursor",long.class,long.class,int.class,int.class);
        fetch.invoke(bridge,1L,1L,100,130);DataInputStream response=new DataInputStream(new ByteArrayInputStream(frames.toByteArray()));int frame=response.readInt();check(frame<150,"byte budget");response.skipBytes(11);check(!response.readBoolean(),"first fetch incomplete");check(response.readInt()==1,"byte budget must split rows");
        frames.reset();fetch.invoke(bridge,2L,1L,100,130);response=new DataInputStream(new ByteArrayInputStream(frames.toByteArray()));response.readInt();response.skipBytes(11);check(response.readBoolean(),"second fetch complete");check(response.readInt()==1,"pending row retained");check(cursors.isEmpty(),"completed cursor released");
        idle.clear();
        Class<?>[] cursorArgs = {long.class,long.class,sessionType,holderType,Statement.class,ResultSet.class,Set.class};
        Object large=construct(cursorType,cursorArgs,2L,43L,session,holder,statement,rows(new String[]{value}),null);cursors.put(2L,large);
        boolean rejected=false;try{fetch.invoke(bridge,3L,2L,100,32);}catch(InvocationTargetException e){rejected=e.getCause() instanceof SQLException;}check(rejected,"oversized row must fail");check(cursors.isEmpty(),"failed cursor released");
        idle.clear();
        Object cancelled=construct(cursorType,cursorArgs,3L,44L,session,holder,statement,rows(new String[]{value}),null);cursors.put(3L,cancelled);method(main,"cancelRequest",long.class).invoke(bridge,44L);for(int i=0;i<100&&!cursors.isEmpty();i++)Thread.sleep(10);check(cursors.isEmpty(),"cancelled opening request releases cursor");
        idle.clear();idle.push(holder);queryResult[0]=rows(new String[]{value});frames.reset();open.invoke(bridge,45L,9L,"SELECT MiXeD FROM s.T",null,null);
        // Cursor IDs are independent of manual test cursor IDs; the second open is 2.
        method(main,"closeCursor",long.class).invoke(bridge,2L);check(cursors.isEmpty(),"explicit close releases cursor");

        queryResult[0] = rows(new String[]{"001", null, ""}, Types.OTHER);
        frames.reset();
        payload.reset();
        DataOutputStream request = new DataOutputStream(payload);
        request.writeLong(9L);
        byte[] sql = "SELECT MiXeD FROM s.T".getBytes("UTF-8");
        request.writeInt(sql.length);
        request.write(sql);
        request.writeInt(1);
        request.writeInt(0);
        method(main, "openOscarDamengCursor", long.class, DataInputStream.class).invoke(
                bridge, 46L, new DataInputStream(new ByteArrayInputStream(payload.toByteArray())));
        check(cursors.size() == 1, "native text cursor retained");
        frames.reset();
        fetch.invoke(bridge, 4L, 3L, 4, 130);
        response = new DataInputStream(new ByteArrayInputStream(frames.toByteArray()));
        response.readInt();
        response.skipBytes(11);
        check(response.readBoolean() && response.readInt() == 3, "native text fetch completed");
        check(response.readInt() == 1 && response.readByte() == 6 && response.readInt() == 3, "native text length");
        byte[] bits = new byte[3];
        response.readFully(bits);
        check(new String(bits, "UTF-8").equals("001"), "leading zeroes preserved");
        check(response.readInt() == 1 && response.readByte() == 0, "native null preserved");
        check(response.readInt() == 1 && response.readByte() == 6 && response.readInt() == 0, "native empty value preserved");

        // Regression: Statement.cancel does nothing and fetch only exits after
        // Future.cancel interrupts ResultSet.next. The input thread must not
        // synchronously wait for the cursor monitor before sending interrupt.
        idle.clear();
        final CountDownLatch entered=new CountDownLatch(1);
        final ResultSetMetaData blockingMeta=(ResultSetMetaData)proxy(ResultSetMetaData.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[]a){if(m.getName().equals("getColumnCount"))return 1;if(m.getName().equals("getColumnType"))return Types.VARCHAR;return null;}});
        ResultSet blocking=(ResultSet)proxy(ResultSet.class,new InvocationHandler(){public Object invoke(Object p,Method m,Object[]a)throws Exception{
            if(m.getName().equals("getMetaData"))return blockingMeta;
            if(m.getName().equals("next")){entered.countDown();try{new CountDownLatch(1).await();}catch(InterruptedException expected){}return false;}
            if(m.getName().equals("close")){closed++;return null;}return null;
        }});
        Object waiting=construct(cursorType,cursorArgs,4L,77L,session,holder,statement,blocking,null);cursors.put(4L,waiting);
        final Object targetBridge=bridge;final Method targetFetch=fetch;
        FutureTask<Void> future=new FutureTask<Void>(new Runnable(){public void run(){try{targetFetch.invoke(targetBridge,99L,4L,100,130);}catch(Exception e){throw new RuntimeException(e);}}},null);
        Field requestsField=main.getDeclaredField("requests");requestsField.setAccessible(true);((Map<Long,Future<?>>)requestsField.get(bridge)).put(99L,future);
        Thread fetching=new Thread(future);fetching.start();check(entered.await(2,TimeUnit.SECONDS),"blocking fetch entered");
        long before=System.nanoTime();method(main,"cancelRequest",long.class).invoke(bridge,99L);check(System.nanoTime()-before<TimeUnit.MILLISECONDS.toNanos(500),"cancel blocks protocol input thread");fetching.join(2000);check(!fetching.isAlive(),"fetch interrupted before cursor cleanup");
        for(int i=0;i<100&&!cursors.isEmpty();i++)Thread.sleep(10);check(cursors.isEmpty(),"cancelled fetch releases cursor");
        method(main,"closeEverything").invoke(bridge);check(closed>=5,"all result sets closed");
    }
}
