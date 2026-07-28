// hatest -- a standalone HA-correctness test for a two-node pg-guard
// cluster. Writes real, persistent rows continuously to whichever node
// currently reports primary, keeps writing across an *externally*
// triggered failover (switchover/promote/kill -- this tool has no
// pg-guard API access, only two plain Postgres connections), and reports
// what actually survived: rows the client believes it committed but that
// vanished ("lost"), gaps in the sequence that aren't explained by a
// failed write ("unexplained gaps" -- real corruption, not expected
// loss), and the unavailability windows it observed (a rough RTO
// measurement).
//
// General-purpose, not Traveler-specific: owns its own table
// (hatest_data) and works against any two-node Postgres/pg-guard cluster.
// A separate, narrower tool from ../traveler/PgTravelerProbe.java, which
// answers "does a *new* connection find the current primary quickly" --
// this one answers "what does an actual failover cost an application with
// data already in flight". See README.md.
//
// pg-guard's replication is async (no synchronous_standby_names anywhere
// in pg-guard's own source), so a handful of the most recent writes being
// lost at the exact instant of a promotion is expected physics, not a
// bug -- this tool reports loss factually rather than asserting it
// should always be zero.
//
// One file, no build tool beyond javac -- pgJDBC is the only dependency,
// referenced directly on the classpath, same as PgTravelerProbe.java.
// NodeConn and Report are top-level classes (not nested inside HaTest),
// package-private, both defined further down in this same file -- javac
// still produces one file per class either way, but this keeps the
// output plainly named (NodeConn.class, Report.class) instead of
// inner-class-qualified (HaTest$NodeConn.class, HaTest$Report.class).
//
//   ../bin/download-postgresql-jdbc.sh
//   javac -cp ../bin/postgresql-jdbc.jar HaTest.java
//   java  -cp .:../bin/postgresql-jdbc.jar HaTest run --node1 HOST:PORT --node2 HOST:PORT --user USER

import java.security.SecureRandom;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.Set;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

public final class HaTest {
    private HaTest() {}

    public static void main(String[] args) throws Exception {
        if (args.length == 0 || "-h".equals(args[0]) || "--help".equals(args[0])) {
            usageAndExit(0);
        }
        if (!"run".equals(args[0])) {
            System.err.println("Unknown command: " + args[0]);
            usageAndExit(1);
        }

        Map<String, String> opt = new LinkedHashMap<>();
        for (int i = 1; i < args.length; i++) {
            String arg = args[i];
            if ("--json".equals(arg)) {
                opt.put("json", "true");
            } else if (arg.startsWith("--")) {
                if (i + 1 >= args.length) throw new IllegalArgumentException("Missing value for " + arg);
                opt.put(arg.substring(2), args[++i]);
            } else {
                throw new IllegalArgumentException("Unexpected argument: " + arg);
            }
        }

        String node1Addr = require(opt, "node1");
        String node2Addr = require(opt, "node2");
        String user = require(opt, "user");
        String password = opt.getOrDefault("password", "");
        String dbname = opt.getOrDefault("dbname", "postgres");
        String sslmode = opt.getOrDefault("sslmode", "disable");
        String sslrootcert = opt.getOrDefault("sslrootcert", "");
        int durationSec = Integer.parseInt(opt.getOrDefault("duration-sec", "120"));
        long writeIntervalMs = Long.parseLong(opt.getOrDefault("write-interval-ms", "200"));
        boolean jsonOut = Boolean.parseBoolean(opt.getOrDefault("json", "false"));

        Properties props = new Properties();
        props.setProperty("user", user);
        props.setProperty("password", password);
        props.setProperty("ApplicationName", "hatest");

        NodeConn n1 = new NodeConn("node1", node1Addr, dbname, sslmode, sslrootcert);
        NodeConn n2 = new NodeConn("node2", node2Addr, dbname, sslmode, sslrootcert);

        // Graceful Ctrl+C: the hook sets the flag and then blocks until the
        // main thread finishes its own graceful shutdown (prints the
        // report, closes connections) -- without that wait, the JVM's
        // default behavior is to halt as soon as every shutdown hook
        // *returns*, which would kill the process mid-report instead of
        // letting it finish cleanly. Shutdown hooks run on *any* JVM exit,
        // not just an external signal -- including the normal
        // System.exit() at the bottom of this method -- so the
        // done.getCount() check matters: without it, a completely normal,
        // successful run would print a misleading "interrupt received"
        // line right after its own report, since by that point main() has
        // already counted the latch down and finished on its own.
        AtomicBoolean interrupted = new AtomicBoolean(false);
        CountDownLatch done = new CountDownLatch(1);
        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            if (done.getCount() > 0) {
                interrupted.set(true);
                System.err.println("[hatest] interrupt received, stopping write loop...");
            }
            try {
                done.await(30, TimeUnit.SECONDS);
            } catch (InterruptedException ignored) {
            }
        }));

        Report rep = runTest(n1, n2, props, durationSec, writeIntervalMs, interrupted);

        n1.closeQuietly();
        n2.closeQuietly();

        if (jsonOut) {
            System.out.println(rep.toJson());
        } else {
            rep.printText();
        }

        done.countDown();
        System.exit(rep.ok ? 0 : 1);
    }

    private static String require(Map<String, String> opt, String key) {
        String v = opt.get(key);
        if (v == null || v.isBlank()) {
            System.err.println("--" + key + " is required");
            usageAndExit(1);
        }
        return v;
    }

    private static void usageAndExit(int code) {
        System.out.println("""
                Usage:
                  java -cp .:postgresql-<version>.jar HaTest run --node1 HOST:PORT --node2 HOST:PORT --user USER [options]

                Required:
                  --node1 HOST:PORT   first node's Postgres address (same value that would
                                      appear inside a JDBC multi-host URL's host list)
                  --node2 HOST:PORT   second node's Postgres address
                  --user USER

                Options:
                  --dbname NAME               default: postgres
                  --password PASSWORD
                  --sslmode MODE              disable|prefer|require|verify-ca|verify-full, default: disable
                  --sslrootcert PATH          CA cert, e.g. ../docker/tls/ca.crt
                  --duration-sec N            default: 120
                  --write-interval-ms N       default: 200
                  --json                      print the report as JSON instead of text

                hatest owns its own table (hatest_data) and creates it if missing -- it
                does not touch any application schema. Trigger the failover yourself
                (curl .../api/switchover, docker compose restart, etc.) while a run is in
                progress; hatest only ever talks to Postgres directly, never to pg-guard's
                own API.
                """);
        System.exit(code);
    }

    /**
     * Polls both nodes and returns whichever currently reports primary.
     * Distinguishes "nobody's primary right now" (mid-promotion, or both
     * unreachable -- the expected transient state during a failover) from
     * "both report primary" (a real split-brain, appended to errsOut as
     * its own distinct entry -- pg-guard's own promote guard exists
     * specifically to prevent this, so seeing it here would be a genuine,
     * separate bug worth its own report).
     */
    static NodeConn findPrimary(NodeConn n1, NodeConn n2, Properties props, List<String> errsOut) {
        List<NodeConn> primaries = new ArrayList<>();
        for (NodeConn n : new NodeConn[] {n1, n2}) {
            try {
                if ("primary".equals(n.role(props))) primaries.add(n);
            } catch (SQLException e) {
                errsOut.add(n.label + ": " + compact(e.getMessage()));
            }
        }
        if (primaries.size() == 1) return primaries.get(0);
        if (primaries.size() > 1) {
            errsOut.add("split-brain: " + primaries.size() + " nodes report primary simultaneously");
        }
        return null;
    }

    // -----------------------------------------------------------------
    // table / writes / validation
    // -----------------------------------------------------------------

    private static final String CREATE_TABLE_SQL = """
            CREATE TABLE IF NOT EXISTS hatest_data (
                run_id     text NOT NULL,
                local_seq  bigint NOT NULL,
                written_at timestamptz NOT NULL DEFAULT now(),
                node       text NOT NULL,
                value      text NOT NULL,
                PRIMARY KEY (run_id, local_seq)
            )""";

    static void ensureTable(NodeConn n, Properties props) throws SQLException {
        Connection c = n.ensure(props);
        try (Statement st = c.createStatement()) {
            st.setQueryTimeout(5);
            st.execute(CREATE_TABLE_SQL);
        }
    }

    static void writeRow(NodeConn n, Properties props, String runId, long localSeq) throws SQLException {
        Connection c = n.ensure(props);
        try (PreparedStatement ps = c.prepareStatement(
                "INSERT INTO hatest_data (run_id, local_seq, node, value) VALUES (?, ?, ?, ?)")) {
            ps.setQueryTimeout(3);
            ps.setString(1, runId);
            ps.setLong(2, localSeq);
            ps.setString(3, n.label);
            ps.setString(4, randomHex(16));
            ps.executeUpdate();
        }
    }

    static Set<Long> queryPresentSeqs(NodeConn n, Properties props, String runId) throws SQLException {
        Connection c = n.ensure(props);
        try (PreparedStatement ps = c.prepareStatement("SELECT local_seq FROM hatest_data WHERE run_id = ?")) {
            ps.setQueryTimeout(10);
            ps.setString(1, runId);
            try (ResultSet rs = ps.executeQuery()) {
                Set<Long> present = new HashSet<>();
                while (rs.next()) present.add(rs.getLong(1));
                return present;
            }
        }
    }

    private static final SecureRandom RANDOM = new SecureRandom();

    static String randomHex(int bytes) {
        byte[] b = new byte[bytes];
        RANDOM.nextBytes(b);
        return HexFormat.of().formatHex(b);
    }

    static String compact(String message) {
        return message == null ? "" : message.replace('\r', ' ').replace('\n', ' ').trim();
    }

    // -----------------------------------------------------------------
    // the test itself
    // -----------------------------------------------------------------

    /**
     * The whole test: write on whichever node is primary until duration
     * elapses (or Ctrl+C), tracking every locally-acknowledged write, then
     * validates what's actually durable. A write that never gets an ack
     * (timeout, read-only-transaction error mid-promotion) is not counted
     * as committed, so a gap at that seq is expected and not reported as
     * loss or corruption -- only a seq the client *did* get committed but
     * that's since missing counts as "lost".
     */
    static Report runTest(NodeConn n1, NodeConn n2, Properties props, int durationSec, long writeIntervalMs, AtomicBoolean interrupted) {
        String runId = randomHex(8);
        long startedMs = System.currentTimeMillis();
        long deadlineMs = startedMs + durationSec * 1000L;

        System.err.printf("[hatest] run_id=%s duration=%ds write_interval=%dms%n", runId, durationSec, writeIntervalMs);

        // Loud, immediate connectivity check. If NEITHER node is reachable
        // at all right now, fail fast here instead of grinding through the
        // full duration retrying against something that was never up --
        // that wastes minutes proving nothing new after the first attempt
        // already showed it isn't working. This only applies to the
        // *initial* check; once a run has found a primary at least once, a
        // later outage during the run is exactly what this tool exists to
        // measure, and the write loop below still waits those out properly.
        List<String> initErrs = new ArrayList<>();
        NodeConn initialPrimary = findPrimary(n1, n2, props, initErrs);
        if (initialPrimary == null) {
            String errs = String.join("; ", initErrs);
            System.err.println("[hatest] FAIL: no primary reachable (" + errs + ")");
            System.err.println("[hatest] is the cluster up? (\"docker compose up -d\" in docker/), and can this host actually reach it?");
            Report rep = new Report();
            rep.runId = runId;
            rep.startedMs = startedMs;
            rep.finishedMs = System.currentTimeMillis();
            rep.ok = false;
            rep.note = "aborted: no primary reachable at startup: " + errs;
            return rep;
        }
        System.err.println("[hatest] initial primary: " + initialPrimary.label);

        Map<Long, Boolean> committed = new LinkedHashMap<>();
        long attempted = 0;
        List<long[]> outages = new ArrayList<>();
        Long outageStartMs = null;
        boolean tableEnsured = false;
        long lastHeartbeatMs = startedMs;

        while (System.currentTimeMillis() < deadlineMs && !interrupted.get()) {
            try {
                Thread.sleep(writeIntervalMs);
            } catch (InterruptedException e) {
                break;
            }
            if (interrupted.get()) {
                System.err.println("[hatest] interrupted, stopping write loop");
                break;
            }

            long nowMs = System.currentTimeMillis();
            // Independent of the write cadence: a periodic proof-of-life so
            // a long silent stretch (writing successfully with nothing to
            // report, or stuck in an outage) is never mistaken for the
            // process having hung.
            if (nowMs - lastHeartbeatMs >= 5000) {
                lastHeartbeatMs = nowMs;
                double elapsedS = (nowMs - startedMs) / 1000.0;
                if (outageStartMs != null) {
                    System.err.printf("[hatest] heartbeat t+%.0fs: still no primary available (%.0fs into this outage) -- committed=%d so far%n",
                            elapsedS, (nowMs - outageStartMs) / 1000.0, committed.size());
                } else {
                    System.err.printf("[hatest] heartbeat t+%.0fs: writing normally -- committed=%d so far%n", elapsedS, committed.size());
                }
            }

            attempted++;
            long localSeq = attempted;

            List<String> errs = new ArrayList<>();
            NodeConn primary = findPrimary(n1, n2, props, errs);
            if (primary == null) {
                if (outageStartMs == null) {
                    outageStartMs = System.currentTimeMillis();
                    System.err.println("[hatest] no primary available (" + String.join("; ", errs) + ") -- entering outage window");
                }
                continue;
            }
            if (outageStartMs != null) {
                long endMs = System.currentTimeMillis();
                outages.add(new long[] {outageStartMs, endMs});
                System.err.printf("[hatest] primary available again (%s) after %.1fs outage%n", primary.label, (endMs - outageStartMs) / 1000.0);
                outageStartMs = null;
            }

            if (!tableEnsured) {
                try {
                    ensureTable(primary, props);
                    tableEnsured = true;
                } catch (SQLException e) {
                    System.err.println("[hatest] creating hatest_data failed: " + compact(e.getMessage()));
                    continue;
                }
            }

            try {
                writeRow(primary, props, runId, localSeq);
                committed.put(localSeq, true);
            } catch (SQLException e) {
                System.err.println("[hatest] write seq=" + localSeq + " failed: " + compact(e.getMessage()));
                // Conservative: treat any write failure as "connection
                // might be bad" and force a reconnect next time, even
                // though most failures here are actually "read-only
                // transaction" (mid-promotion), not a dead connection --
                // ensure()'s validity check will no-op if the connection
                // is actually still fine.
                primary.closeQuietly();
            }
        }

        if (outageStartMs != null) {
            outages.add(new long[] {outageStartMs, System.currentTimeMillis()});
        }

        Report rep = new Report();
        rep.runId = runId;
        rep.startedMs = startedMs;
        rep.finishedMs = System.currentTimeMillis();
        rep.attempted = attempted;
        rep.committedByClient = committed.size();
        rep.outages = outages;
        for (long[] o : outages) rep.totalOutageSecs += (o[1] - o[0]) / 1000.0;

        // Validation pass: give replication/reconnect a moment to settle,
        // then read back whichever node is primary right now.
        try {
            Thread.sleep(500);
        } catch (InterruptedException ignored) {
        }
        List<String> valErrs = new ArrayList<>();
        NodeConn validatePrimary = findPrimary(n1, n2, props, valErrs);
        if (validatePrimary == null) {
            rep.note = "could not validate: " + String.join("; ", valErrs);
            rep.ok = false;
            return rep;
        }
        Set<Long> present;
        try {
            present = queryPresentSeqs(validatePrimary, props, runId);
        } catch (SQLException e) {
            rep.note = "could not query hatest_data for validation: " + compact(e.getMessage());
            rep.ok = false;
            return rep;
        }
        rep.presentInDb = present.size();

        List<Long> lost = new ArrayList<>();
        List<Long> gaps = new ArrayList<>();
        for (long seq = 1; seq <= attempted; seq++) {
            boolean wasCommitted = committed.containsKey(seq);
            boolean isPresent = present.contains(seq);
            if (wasCommitted && !isPresent) {
                lost.add(seq);
            } else if (!wasCommitted && !isPresent) {
                rep.notAcknowledged++;
            } else if (!wasCommitted && isPresent) {
                // Write actually succeeded server-side but the ack never
                // made it back to the client (e.g. connection dropped
                // right after COMMIT) -- ambiguous by nature, not loss or
                // corruption.
                gaps.add(seq);
            }
        }
        rep.lost = lost;
        rep.unexplainedGaps = gaps;
        rep.ok = lost.isEmpty();
        if (!gaps.isEmpty()) {
            rep.note = "unexplained_gap_seqs are rows present in the DB whose commit ack never reached the client (not loss or corruption, just an ambiguous ack -- see README)";
        }
        return rep;
    }
}

// -----------------------------------------------------------------
// per-node connection
// -----------------------------------------------------------------

/** A single lazily-(re)connected JDBC connection to one cluster member. */
final class NodeConn {
    final String label;
    final String url;
    Connection conn;

    NodeConn(String label, String hostport, String dbname, String sslmode, String sslrootcert) {
        this.label = label + " (" + hostport + ")";
        StringBuilder u = new StringBuilder("jdbc:postgresql://").append(hostport).append('/').append(dbname);
        List<String> params = new ArrayList<>();
        params.add("connectTimeout=3");
        params.add("loginTimeout=5");
        params.add("socketTimeout=10");
        if (!sslmode.isBlank()) params.add("sslmode=" + sslmode);
        if (!sslrootcert.isBlank()) params.add("sslrootcert=" + sslrootcert);
        u.append('?').append(String.join("&", params));
        this.url = u.toString();
    }

    /** Reconnects from scratch if the cached connection is gone or fails a quick validity check. */
    Connection ensure(Properties props) throws SQLException {
        if (conn != null) {
            try {
                if (conn.isValid(2)) return conn;
            } catch (SQLException ignored) {
            }
            closeQuietly();
        }
        System.err.println("[hatest] " + label + ": connecting...");
        long start = System.nanoTime();
        try {
            conn = DriverManager.getConnection(url, props);
        } catch (SQLException e) {
            System.err.println("[hatest] " + label + ": connect failed after " + msSince(start) + "ms: " + HaTest.compact(e.getMessage()));
            throw e;
        }
        System.err.println("[hatest] " + label + ": connected in " + msSince(start) + "ms");
        return conn;
    }

    String role(Properties props) throws SQLException {
        Connection c = ensure(props);
        try (Statement st = c.createStatement()) {
            st.setQueryTimeout(3);
            try (ResultSet rs = st.executeQuery("SELECT pg_is_in_recovery()")) {
                rs.next();
                return rs.getBoolean(1) ? "standby" : "primary";
            }
        } catch (SQLException e) {
            closeQuietly();
            throw e;
        }
    }

    void closeQuietly() {
        if (conn != null) {
            try {
                conn.close();
            } catch (SQLException ignored) {
            }
            conn = null;
        }
    }

    private static long msSince(long startNanos) {
        return (System.nanoTime() - startNanos) / 1_000_000;
    }
}

// -----------------------------------------------------------------
// report
// -----------------------------------------------------------------

final class Report {
    String runId;
    long startedMs;
    long finishedMs;
    long attempted;
    long committedByClient;
    long presentInDb;
    List<Long> lost = List.of();
    List<Long> unexplainedGaps = List.of();
    long notAcknowledged;
    List<long[]> outages = List.of();
    double totalOutageSecs;
    boolean ok;
    String note;

    void printText() {
        System.out.println("HA round-trip test report");
        System.out.println("  run_id             = " + runId);
        System.out.println("  started            = " + Instant.ofEpochMilli(startedMs));
        System.out.printf("  finished           = %s (%.1fs)%n", Instant.ofEpochMilli(finishedMs), (finishedMs - startedMs) / 1000.0);
        System.out.println("  writes attempted   = " + attempted);
        System.out.println("  writes committed   = " + committedByClient);
        System.out.println("  present in DB      = " + presentInDb);
        System.out.println("  not acknowledged   = " + notAcknowledged + " (write failed/timed out client-side, e.g. during an outage window)");
        System.out.print("  lost (committed but missing) = " + lost.size());
        if (!lost.isEmpty()) System.out.print(" " + lost);
        System.out.println();
        System.out.print("  unexplained gaps   = " + unexplainedGaps.size());
        if (!unexplainedGaps.isEmpty()) System.out.print(" " + unexplainedGaps);
        System.out.println();
        System.out.printf("  outage windows     = %d (total %.1fs)%n", outages.size(), totalOutageSecs);
        for (long[] o : outages) {
            System.out.printf("    - %s -> %s (%.1fs)%n", Instant.ofEpochMilli(o[0]), Instant.ofEpochMilli(o[1]), (o[1] - o[0]) / 1000.0);
        }
        if (note != null) System.out.println("  note               = " + note);
        System.out.println("  result             = " + (ok ? "PASS" : "FAIL"));
    }

    String toJson() {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"run_id\":\"").append(jsonEscape(runId)).append("\",");
        sb.append("\"started\":\"").append(Instant.ofEpochMilli(startedMs)).append("\",");
        sb.append("\"finished\":\"").append(Instant.ofEpochMilli(finishedMs)).append("\",");
        sb.append("\"writes_attempted\":").append(attempted).append(',');
        sb.append("\"writes_committed\":").append(committedByClient).append(',');
        sb.append("\"present_in_db\":").append(presentInDb).append(',');
        sb.append("\"lost_seqs\":").append(longListJson(lost)).append(',');
        sb.append("\"unexplained_gap_seqs\":").append(longListJson(unexplainedGaps)).append(',');
        sb.append("\"not_acknowledged\":").append(notAcknowledged).append(',');
        sb.append("\"outages\":[");
        for (int i = 0; i < outages.size(); i++) {
            if (i > 0) sb.append(',');
            long[] o = outages.get(i);
            sb.append("{\"start\":\"").append(Instant.ofEpochMilli(o[0])).append("\",");
            sb.append("\"end\":\"").append(Instant.ofEpochMilli(o[1])).append("\",");
            sb.append("\"seconds\":").append((o[1] - o[0]) / 1000.0).append('}');
        }
        sb.append("],");
        sb.append("\"total_outage_seconds\":").append(totalOutageSecs).append(',');
        sb.append("\"ok\":").append(ok);
        if (note != null) sb.append(",\"note\":\"").append(jsonEscape(note)).append('"');
        sb.append('}');
        return sb.toString();
    }

    private static String longListJson(List<Long> vs) {
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < vs.size(); i++) {
            if (i > 0) sb.append(',');
            sb.append(vs.get(i));
        }
        return sb.append(']').toString();
    }

    private static String jsonEscape(String value) {
        if (value == null) return "";
        StringBuilder out = new StringBuilder();
        for (char c : value.toCharArray()) {
            switch (c) {
                case '"' -> out.append("\\\"");
                case '\\' -> out.append("\\\\");
                case '\n' -> out.append("\\n");
                case '\r' -> out.append("\\r");
                case '\t' -> out.append("\\t");
                default -> out.append(c);
            }
        }
        return out.toString();
    }
}
