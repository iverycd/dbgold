import java.io.*;
import java.nio.charset.StandardCharsets;
import java.security.*;
import java.sql.*;

/** Real-vendor readback without the bridge cursor's per-row size limit. */
public final class OscarLOBRoundTripProbe {
    private static String hex(byte[] bytes) {
        StringBuilder result = new StringBuilder();
        for (byte b : bytes) result.append(String.format("%02x", b & 255));
        return result.toString();
    }
    private static void check(boolean ok, String message) {
        if (!ok) throw new AssertionError(message);
    }
    private static String textHash(Reader input) throws Exception {
        check(input != null, "unexpected NULL CLOB");
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        OutputStream sink = new OutputStream() { public void write(int b) { } public void write(byte[] b, int o, int n) { } };
        try (Reader reader = input;
             Writer writer = new OutputStreamWriter(new DigestOutputStream(sink, digest), StandardCharsets.UTF_8.newEncoder())) {
            char[] buffer = new char[8192];
            int n;
            while ((n = reader.read(buffer)) != -1) writer.write(buffer, 0, n);
        }
        return hex(digest.digest());
    }
    private static String binaryHash(InputStream input) throws Exception {
        check(input != null, "unexpected NULL BLOB");
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        try (InputStream stream = input) {
            byte[] buffer = new byte[8192];
            int n;
            while ((n = stream.read(buffer)) != -1) digest.update(buffer, 0, n);
        }
        return hex(digest.digest());
    }
    public static void main(String[] args) throws Exception {
        Class.forName("com.oscar.Driver");
        try (Connection connection = DriverManager.getConnection(System.getenv("OSCAR_TEST_URL"), System.getenv("OSCAR_TEST_USER"), System.getenv("OSCAR_TEST_PASSWORD"));
             Statement statement = connection.createStatement()) {
            try (ResultSet row = statement.executeQuery("SELECT \"FLAG\",\"BITS64\",\"WARNINGMSG\",\"BODY\",\"PAYLOAD\" FROM " + args[0] + " WHERE \"ID\"=1")) {
                check(row.next(), "missing migrated row");
                check("1".equals(row.getString(1)), "BIT value changed");
                check("18446744073709551615".equals(row.getBigDecimal(2).toPlainString()), "BIT(64) value changed");
                check(args[1].equals(textHash(row.getCharacterStream(3))), "VARCHAR(10000) to CLOB content changed");
                check(args[2].equals(textHash(row.getCharacterStream(4))), "large CLOB content changed");
                check(args[3].equals(binaryHash(row.getBinaryStream(5))), "large BLOB content changed");
            }
            try (ResultSet row = statement.executeQuery("SELECT \"FLAG\",\"BITS64\" FROM " + args[0] + " WHERE \"ID\"=2")) {
                check(row.next(), "missing default row");
                check("10".equals(row.getString(1)), "BIT default changed");
                check("18446744073709551615".equals(row.getBigDecimal(2).toPlainString()), "BIT(64) default changed");
            }
        }
    }
}
