// pgjdbc-ha-probe -- a small diagnostic client for a two-node PostgreSQL
// setup. Connects the way a JDBC client application (like Traveler) would,
// using pgJDBC's own multi-host HA URL syntax, and reports which physical
// node it actually landed on.
//
// One class, no build tool: pgJDBC is the only dependency, referenced on
// the classpath directly.
//
//   ../bin/download-postgresql-jdbc.sh
//   javac -cp ../bin/postgresql-jdbc.jar PgTravelerProbe.java
//   java  -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe probe --url "$PGTEST_URL"      (Linux/macOS)
//   java  -cp ".;../bin/postgresql-jdbc.jar" PgTravelerProbe probe --url "%PGTEST_URL%"   (Windows)
//
// See README.md for commands, formats, and the recommended JDBC URL.

import com.sun.net.httpserver.Headers;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.time.Duration;
import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.Properties;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;

public final class PgTravelerProbe {
    private PgTravelerProbe() {}

    public static void main(String[] args) throws Exception {
        if (args.length == 0 || "--help".equals(args[0]) || "-h".equals(args[0])) {
            usageAndExit(0);
        }

        String command = args[0];
        if (!command.equals("probe") && !command.equals("watch") && !command.equals("serve")) {
            System.err.println("Unknown command: " + command);
            usageAndExit(1);
        }

        Map<String, String> opt = new LinkedHashMap<>();
        for (int i = 1; i < args.length; i++) {
            String arg = args[i];
            if ("--write-test".equals(arg)) {
                opt.put("write-test", "true");
            } else if (arg.startsWith("--")) {
                if (i + 1 >= args.length) throw new IllegalArgumentException("Missing value for " + arg);
                opt.put(arg.substring(2), args[++i]);
            } else {
                throw new IllegalArgumentException("Unexpected argument: " + arg);
            }
        }

        System.err.println("[pgjdbc-ha-probe] command = " + command);
        String url = resolveAndLog(opt, "url", "PGTEST_URL", "", false);
        if (url.isBlank()) throw new IllegalArgumentException("JDBC URL required via --url or PGTEST_URL");
        String user = resolveAndLog(opt, "user", "PGTEST_USER", "", false);
        String password = resolveAndLog(opt, "password", "PGTEST_PASSWORD", "", true);
        String applicationName = resolveAndLog(opt, "application-name", "PGTEST_APPLICATION_NAME", "pgjdbc-ha-probe", false);
        String format = opt.getOrDefault("format", "text");
        boolean writeTest = Boolean.parseBoolean(opt.getOrDefault("write-test", "false"));
        long intervalMs = Long.parseLong(opt.getOrDefault("interval-ms", "1000"));
        String bind = opt.getOrDefault("bind", "127.0.0.1");
        int port = Integer.parseInt(opt.getOrDefault("port", "9187"));
        System.err.println("[pgjdbc-ha-probe] format = " + format
                + ", write-test = " + writeTest
                + ", interval-ms = " + intervalMs
                + (command.equals("serve") ? ", bind = " + bind + ", port = " + port : ""));

        switch (command) {
            case "probe" -> {
                Map<String, Object> result = probe(url, user, password, applicationName, writeTest);
                System.out.print(format(result, format));
                System.exit(Boolean.TRUE.equals(result.get("ok")) ? 0 : 2);
            }
            case "watch" -> {
                while (!Thread.currentThread().isInterrupted()) {
                    Map<String, Object> result = probe(url, user, password, applicationName, writeTest);
                    String body = format(result, format);
                    System.out.print(body);
                    if (!body.endsWith("\n")) System.out.println();
                    Thread.sleep(intervalMs);
                }
            }
            case "serve" -> serve(url, user, password, applicationName, writeTest, format, intervalMs, bind, port);
        }
    }

    // -------------------------------------------------------------------
    // serve mode
    // -------------------------------------------------------------------

    private static void serve(
            String url, String user, String password, String applicationName, boolean writeTest,
            String defaultFormat, long intervalMs, String bind, int port) throws IOException {
        AtomicReference<Map<String, Object>> last = new AtomicReference<>(notRun("No probe has run yet"));

        ScheduledExecutorService scheduler = Executors.newSingleThreadScheduledExecutor(r -> {
            Thread t = new Thread(r, "pg-probe-scheduler");
            t.setDaemon(true);
            return t;
        });

        Runnable refresh = () -> last.set(probe(url, user, password, applicationName, writeTest));
        refresh.run();
        scheduler.scheduleWithFixedDelay(refresh, intervalMs, intervalMs, TimeUnit.MILLISECONDS);

        HttpServer server = HttpServer.create(new InetSocketAddress(bind, port), 32);
        server.setExecutor(Executors.newCachedThreadPool());

        server.createContext("/health/live", exchange ->
                respond(exchange, 200, "text/plain; charset=utf-8", "ok\n", null));

        server.createContext("/v1/status", exchange -> {
            if (!"GET".equalsIgnoreCase(exchange.getRequestMethod())) {
                methodNotAllowed(exchange);
                return;
            }
            String requested = query(exchange.getRequestURI()).getOrDefault("format", defaultFormat);
            respondFormatted(exchange, last.get(), requested);
        });

        server.createContext("/v1/probe", exchange -> {
            if (!"GET".equalsIgnoreCase(exchange.getRequestMethod())
                    && !"POST".equalsIgnoreCase(exchange.getRequestMethod())) {
                methodNotAllowed(exchange);
                return;
            }
            Map<String, String> q = query(exchange.getRequestURI());
            String requested = q.getOrDefault("format", defaultFormat);
            boolean wt = Boolean.parseBoolean(q.getOrDefault("writeTest", Boolean.toString(writeTest)));
            Map<String, Object> result = probe(url, user, password, applicationName, wt);
            last.set(result);
            respondFormatted(exchange, result, requested);
        });

        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            server.stop(1);
            scheduler.shutdownNow();
        }, "pg-probe-shutdown"));

        server.start();
        System.out.printf("pgjdbc-ha-probe listening on http://%s:%d%n", bind, port);
    }

    private static void respondFormatted(HttpExchange exchange, Map<String, Object> result, String requestedFormat)
            throws IOException {
        String normalized = normalizeFormat(requestedFormat);
        String body = format(result, normalized);
        String contentType = switch (normalized) {
            case "json" -> "application/json; charset=utf-8";
            default -> "text/plain; charset=utf-8";
        };

        Headers extra = new Headers();
        result.forEach((key, value) -> {
            if (value != null) extra.set("X-PG-PROBE-" + toHeaderName(key), sanitizeHeaderValue(String.valueOf(value)));
        });
        extra.set("Cache-Control", "no-store");
        respond(exchange, Boolean.TRUE.equals(result.get("ok")) ? 200 : 503, contentType, body, extra);
    }

    // -------------------------------------------------------------------
    // the probe itself
    // -------------------------------------------------------------------

    private static Map<String, Object> probe(
            String url, String user, String password, String applicationName, boolean writeTest) {
        Instant started = Instant.now();
        long startedNanos = System.nanoTime();

        Properties properties = new Properties();
        if (!user.isBlank()) properties.setProperty("user", user);
        if (!password.isBlank()) properties.setProperty("password", password);
        properties.setProperty("ApplicationName", applicationName);

        Map<String, Object> r = new LinkedHashMap<>();
        r.put("timestamp", started.toString());

        try (Connection connection = DriverManager.getConnection(url, properties)) {
            long connectMs = Duration.ofNanos(System.nanoTime() - startedNanos).toMillis();

            String sql = """
                    SELECT
                      COALESCE(inet_server_addr()::text, ''),
                      inet_server_port(),
                      pg_backend_pid(),
                      pg_is_in_recovery(),
                      current_setting('transaction_read_only')::boolean,
                      current_setting('server_version'),
                      COALESCE(current_setting('cluster_name', true), '')
                    """;

            boolean inRecovery;
            try (Statement statement = connection.createStatement();
                 ResultSet rs = statement.executeQuery(sql)) {
                if (!rs.next()) throw new SQLException("Probe query returned no row");
                r.put("server_address", rs.getString(1));
                r.put("server_port", rs.getInt(2));
                r.put("backend_pid", rs.getInt(3));
                inRecovery = rs.getBoolean(4);
                r.put("in_recovery", inRecovery);
                r.put("transaction_read_only", rs.getBoolean(5));
                r.put("server_version", rs.getString(6));
                r.put("cluster_name", rs.getString(7));
            }
            r.put("role", inRecovery ? "standby" : "primary");

            // pg_stat_ssl reports on *this exact connection*, independent of
            // what sslmode was requested -- e.g. the default "prefer" mode
            // silently uses TLS if the server offers it without the caller
            // ever passing an sslmode flag, so this is the only reliable way
            // to see whether the connection actually ended up encrypted.
            try (Statement statement = connection.createStatement();
                 ResultSet rs = statement.executeQuery(
                         "SELECT ssl, COALESCE(version, ''), COALESCE(cipher, '') "
                                 + "FROM pg_stat_ssl WHERE pid = pg_backend_pid()")) {
                if (rs.next()) {
                    r.put("tls_enabled", rs.getBoolean(1));
                    String tlsVersion = rs.getString(2);
                    String tlsCipher = rs.getString(3);
                    if (!tlsVersion.isBlank()) r.put("tls_version", tlsVersion);
                    if (!tlsCipher.isBlank()) r.put("tls_cipher", tlsCipher);
                }
            } catch (SQLException e) {
                // Not fatal to the probe itself -- just means TLS status is
                // unknown (e.g. insufficient privilege on pg_stat_ssl).
                r.put("tls_enabled", null);
            }

            r.put("write_test_requested", writeTest);
            if (writeTest) {
                try {
                    connection.setAutoCommit(false);
                    try (Statement statement = connection.createStatement()) {
                        statement.execute("CREATE TEMP TABLE pgjdbc_ha_probe_tmp(value integer)");
                        statement.executeUpdate("INSERT INTO pgjdbc_ha_probe_tmp(value) VALUES (1)");
                    }
                    connection.rollback();
                    r.put("write_succeeded", true);
                } catch (SQLException e) {
                    safeRollback(connection);
                    r.put("write_succeeded", false);
                    r.put("write_error", compact(e.getMessage()));
                } finally {
                    try {
                        connection.setAutoCommit(true);
                    } catch (SQLException ignored) {
                        // The connection may have been lost during the test.
                    }
                }
            }

            r.put("ok", true);
            r.put("connect_ms", connectMs);
            r.put("duration_ms", Duration.ofNanos(System.nanoTime() - startedNanos).toMillis());
            return r;
        } catch (SQLException e) {
            r.put("ok", false);
            r.put("duration_ms", Duration.ofNanos(System.nanoTime() - startedNanos).toMillis());
            r.put("role", "unknown");
            r.put("sql_state", e.getSQLState() == null ? "" : e.getSQLState());
            r.put("error", compact(e.getMessage()));
            r.put("vendor_error_code", e.getErrorCode());
            return r;
        }
    }

    private static Map<String, Object> notRun(String message) {
        Map<String, Object> r = new LinkedHashMap<>();
        r.put("ok", false);
        r.put("timestamp", Instant.now().toString());
        r.put("role", "unknown");
        r.put("error", message);
        return r;
    }

    private static void safeRollback(Connection connection) {
        try {
            connection.rollback();
        } catch (SQLException ignored) {
            // Preserve the original failure.
        }
    }

    // -------------------------------------------------------------------
    // formatting
    // -------------------------------------------------------------------

    private static String format(Map<String, Object> r, String format) {
        return switch (normalizeFormat(format)) {
            case "json" -> toJson(r) + "\n";
            case "headers" -> toHeaderText(r);
            default -> toPlainText(r);
        };
    }

    private static String normalizeFormat(String format) {
        if (format == null) return "text";
        return switch (format.toLowerCase(Locale.ROOT)) {
            case "json" -> "json";
            case "headers", "header" -> "headers";
            default -> "text";
        };
    }

    private static String toPlainText(Map<String, Object> r) {
        StringBuilder out = new StringBuilder();
        r.forEach((key, value) -> out.append(key).append('=').append(value).append('\n'));
        return out.toString();
    }

    private static String toHeaderText(Map<String, Object> r) {
        StringBuilder out = new StringBuilder();
        r.forEach((key, value) -> out.append("X-PG-PROBE-").append(toHeaderName(key)).append(": ")
                .append(sanitizeHeaderValue(String.valueOf(value))).append("\r\n"));
        return out.toString();
    }

    private static String toJson(Map<String, Object> r) {
        StringBuilder out = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> entry : r.entrySet()) {
            if (!first) out.append(',');
            first = false;
            out.append('"').append(jsonEscape(entry.getKey())).append("\":");
            Object value = entry.getValue();
            if (value == null) out.append("null");
            else if (value instanceof Number || value instanceof Boolean) out.append(value);
            else out.append('"').append(jsonEscape(String.valueOf(value))).append('"');
        }
        return out.append('}').toString();
    }

    private static String toHeaderName(String input) {
        StringBuilder out = new StringBuilder();
        for (char c : input.toCharArray()) {
            out.append(c == '_' ? '-' : Character.toUpperCase(c));
        }
        return out.toString();
    }

    private static String sanitizeHeaderValue(String value) {
        return value.replace('\r', ' ').replace('\n', ' ');
    }

    private static String jsonEscape(String value) {
        StringBuilder out = new StringBuilder();
        for (char c : value.toCharArray()) {
            switch (c) {
                case '"' -> out.append("\\\"");
                case '\\' -> out.append("\\\\");
                case '\b' -> out.append("\\b");
                case '\f' -> out.append("\\f");
                case '\n' -> out.append("\\n");
                case '\r' -> out.append("\\r");
                case '\t' -> out.append("\\t");
                default -> {
                    if (c < 0x20) out.append(String.format("\\u%04x", (int) c));
                    else out.append(c);
                }
            }
        }
        return out.toString();
    }

    private static String compact(String message) {
        return Objects.requireNonNullElse(message, "").replace('\r', ' ').replace('\n', ' ').trim();
    }

    // -------------------------------------------------------------------
    // HTTP plumbing
    // -------------------------------------------------------------------

    private static void respond(HttpExchange exchange, int status, String contentType, String body, Headers extraHeaders)
            throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", contentType);
        if (extraHeaders != null) extraHeaders.forEach((name, values) -> exchange.getResponseHeaders().put(name, values));
        exchange.sendResponseHeaders(status, bytes.length);
        try (var output = exchange.getResponseBody()) {
            output.write(bytes);
        }
    }

    private static void methodNotAllowed(HttpExchange exchange) throws IOException {
        exchange.getResponseHeaders().set("Allow", "GET, POST");
        respond(exchange, 405, "text/plain; charset=utf-8", "method not allowed\n", null);
    }

    private static Map<String, String> query(URI uri) {
        Map<String, String> result = new LinkedHashMap<>();
        String raw = uri.getRawQuery();
        if (raw == null || raw.isBlank()) return result;
        for (String part : raw.split("&")) {
            String[] kv = part.split("=", 2);
            String key = URLDecoder.decode(kv[0], StandardCharsets.UTF_8);
            String value = kv.length == 2 ? URLDecoder.decode(kv[1], StandardCharsets.UTF_8) : "";
            result.put(key, value);
        }
        return result;
    }

    // -------------------------------------------------------------------
    // CLI plumbing
    // -------------------------------------------------------------------

    // Resolves a value from --flag, then the env var, then the default --
    // and prints which one won, so a stale exported PGTEST_URL silently
    // overriding a fresh --url is visible instead of showing up as just an
    // unexplained wrong connection.
    private static String resolveAndLog(Map<String, String> opt, String key, String env, String def, boolean mask) {
        String value;
        String source;
        if (opt.containsKey(key)) {
            value = opt.get(key);
            source = "--" + key;
        } else if (System.getenv(env) != null) {
            value = System.getenv(env);
            source = env + " (env)";
        } else {
            value = def;
            source = "default";
        }
        String shown = value.isBlank() ? "(empty)" : (mask ? "*".repeat(Math.min(value.length(), 8)) : value);
        System.err.printf("[pgjdbc-ha-probe] %-16s = %-40s (source: %s)%n", key, shown, source);
        return value;
    }

    private static void usageAndExit(int code) {
        System.out.println("""
                Usage:
                  java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe probe [options]
                  java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe watch [options]
                  java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe serve [options]

                Required:
                  --url JDBC_URL
                    or environment variable PGTEST_URL

                Options:
                  --user USER                 or PGTEST_USER
                  --password PASSWORD         or PGTEST_PASSWORD
                  --application-name NAME     default: pgjdbc-ha-probe
                  --format text|json|headers  default: text
                  --write-test                temporary-table write test
                  --interval-ms N             watch/serve interval, default: 1000
                  --bind ADDRESS              serve default: 127.0.0.1
                  --port PORT                 serve default: 9187

                HTTP:
                  GET  /health/live
                  GET  /v1/status?format=json|text|headers
                  GET  /v1/probe?format=json|text|headers&writeTest=true
                  POST /v1/probe?format=json|text|headers&writeTest=true
                """);
        System.exit(code);
    }
}
