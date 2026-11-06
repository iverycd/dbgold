package jdbcbridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	JavaBin         string
	JDBCJar         string
	BridgeJar       string
	LoginTimeoutMS  int
	SocketTimeoutMS int
	MaxHeapMB       int
	TempDir         string
}

type SessionOptions struct {
	URL             string
	Username        string
	Password        string
	Schema          string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type BridgeError struct {
	SQLState string
	Code     int32
	Message  string
}

func (e *BridgeError) Error() string {
	prefix := "oscar JDBC"
	if e.SQLState != "" {
		prefix += " SQLSTATE " + e.SQLState
	}
	if e.Code != 0 {
		prefix += fmt.Sprintf(" code %d", e.Code)
	}
	return prefix + ": " + e.Message
}

type response struct {
	body []byte
	err  error
}

type bridgeStartup struct {
	done chan struct{}
	err  error
}

// Manager owns the single lazy JVM bridge process used by all Oscar sessions.
type Manager struct {
	cfg Config

	processMu  sync.Mutex
	starting   *bridgeStartup
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	generation uint64

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[uint64]chan response
	nextID    atomic.Uint64
	nextLOBID atomic.Uint64
}

func NewManager(cfg Config) *Manager {
	return &Manager{cfg: withDefaults(cfg), pending: make(map[uint64]chan response)}
}

var (
	defaultMu      sync.RWMutex
	defaultManager = NewManager(Config{})
)

func ConfigureDefault(cfg Config) {
	defaultMu.Lock()
	old := defaultManager
	defaultManager = NewManager(cfg)
	defaultMu.Unlock()
	_ = old.Close()
}

func Default() *Manager {
	defaultMu.RLock()
	m := defaultManager
	defaultMu.RUnlock()
	return m
}

func withDefaults(cfg Config) Config {
	if cfg.LoginTimeoutMS <= 0 {
		cfg.LoginTimeoutMS = 15000
	}
	if cfg.MaxHeapMB <= 0 {
		cfg.MaxHeapMB = 512
	}
	return cfg
}

func executableDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

func firstExisting(candidates ...string) string {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func resolveJava(configured string) (string, error) {
	name := strings.TrimSpace(configured)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("OSCAR_JAVA_BIN"))
	}
	if name != "" {
		path, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("神通 JDBC 需要 Java 8 或更高版本，OSCAR_JAVA_BIN=%q 不可执行: %w", name, err)
		}
		return path, nil
	}

	name = "java"
	if runtime.GOOS == "windows" {
		name = "java.exe"
	}
	home := strings.TrimSpace(os.Getenv("JAVA_HOME"))
	if home != "" {
		if path, err := exec.LookPath(filepath.Join(home, "bin", name)); err == nil {
			return path, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("神通 JDBC 需要 Java 8 或更高版本，JAVA_HOME=%q 和 PATH 中均未找到 %s，请设置 OSCAR_JAVA_BIN: %w", home, name, err)
	}
	return path, nil
}

func (m *Manager) resolvePaths() (javaBin, bridgeJar, jdbcJar string, err error) {
	exeDir := executableDir()
	javaBin, err = resolveJava(m.cfg.JavaBin)
	if err != nil {
		return "", "", "", err
	}
	bridgeJar = strings.TrimSpace(m.cfg.BridgeJar)
	if bridgeJar != "" {
		if _, statErr := os.Stat(bridgeJar); statErr != nil {
			return "", "", "", fmt.Errorf("Oscar JDBC runtime unavailable: OSCAR_BRIDGE_JAR %q: %w", bridgeJar, statErr)
		}
	} else {
		bridgeJar = firstExisting(
			filepath.Join(exeDir, "lib", "dbgold-oscar-bridge.jar"),
			filepath.Join("jdbcbridge", "java", "build", "dbgold-oscar-bridge.jar"),
		)
	}
	jdbcJar = strings.TrimSpace(m.cfg.JDBCJar)
	if jdbcJar != "" {
		if _, statErr := os.Stat(jdbcJar); statErr != nil {
			return "", "", "", fmt.Errorf("Oscar JDBC runtime unavailable: OSCAR_JDBC_JAR %q: %w", jdbcJar, statErr)
		}
	} else {
		jdbcJar = firstExisting(
			filepath.Join(exeDir, "lib", "oscarJDBC8.jar"),
			filepath.Join("third_party", "oscar", "oscarJDBC8.jar"),
		)
	}
	var missing []string
	if bridgeJar == "" {
		missing = append(missing, "dbgold-oscar-bridge.jar")
	}
	if jdbcJar == "" {
		missing = append(missing, "oscarJDBC8.jar (OSCAR_JDBC_JAR or bundled lib/oscarJDBC8.jar)")
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("Oscar JDBC runtime unavailable: missing %s", strings.Join(missing, ", "))
	}
	return javaBin, bridgeJar, jdbcJar, nil
}

func (m *Manager) ensureStarted(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.processMu.Lock()
	if starting := m.starting; starting != nil {
		m.processMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-starting.done:
			return starting.err
		}
	}
	if m.cmd != nil && m.cmd.Process != nil {
		m.processMu.Unlock()
		return nil
	}
	// Concurrent callers share this handshake and its diagnostics. A later
	// operation can retry if the customer has corrected their Java setup.
	starting := &bridgeStartup{done: make(chan struct{})}
	m.starting = starting
	defer func() {
		m.processMu.Lock()
		starting.err = err
		m.starting = nil
		close(starting.done)
		m.processMu.Unlock()
	}()
	javaBin, bridgeJar, jdbcJar, err := m.resolvePaths()
	if err != nil {
		m.processMu.Unlock()
		return err
	}
	spoolDir, err := os.MkdirTemp(m.cfg.TempDir, "dbgold-oscar-spool-")
	if err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("create Oscar bridge LOB spool directory: %w", err)
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(spoolDir)
		}
	}()
	absoluteSpoolDir, err := filepath.Abs(spoolDir)
	if err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("resolve Oscar bridge LOB spool directory: %w", err)
	}
	classpath := bridgeJar + string(os.PathListSeparator) + jdbcJar
	args := []string{fmt.Sprintf("-Xmx%dm", m.cfg.MaxHeapMB), "-Ddbgold.oscar.spoolDir=" + absoluteSpoolDir}
	if m.cfg.TempDir != "" {
		args = append(args, "-Djava.io.tmpdir="+m.cfg.TempDir, "-XX:+IgnoreUnrecognizedVMOptions", "-XX:-UsePerfData")
	}
	args = append(args, "-cp", classpath, "com.dbgold.oscar.BridgeMain")
	cmd := exec.Command(javaBin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("create Oscar bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("create Oscar bridge stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("create Oscar bridge stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		m.processMu.Unlock()
		return fmt.Errorf("无法启动神通 JDBC，请检查 %q 是否为本机可运行的 Java 8 或更高版本: %w", javaBin, err)
	}
	started = true
	m.cmd, m.stdin, m.stdout = cmd, stdin, stdout
	m.generation++
	gen := m.generation
	m.processMu.Unlock()

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		m.readLoop(gen, stdout)
	}()
	stderrDone := make(chan struct{})
	reaped := make(chan struct{})
	var diagnostic string
	go func() {
		defer close(reaped)
		diagnostic = readBridgeStderr(stderr)
		close(stderrDone)
		<-stdoutDone // Wait closes the pipes, including any pending final response.
		err := cmd.Wait()
		// Only the JVM owns live LOB files. Once it has been reaped, even an
		// abnormal exit can safely remove every file from this generation.
		if cleanupErr := os.RemoveAll(spoolDir); cleanupErr != nil {
			slog.Warn("Oscar JDBC bridge LOB cleanup failed", "error", cleanupErr)
		}
		m.processDied(gen, fmt.Errorf("Oscar JDBC bridge exited: %w", err))
	}()

	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := m.requestStarted(helloCtx, opHello, nil)
	var driverVersion string
	if err == nil {
		driverVersion, err = decodeDriverVersion(body)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		m.processDied(gen, err)
		// A Java launcher may have left children holding the pipes. Give stderr
		// time to drain, then unblock both readers so Wait can reap the launcher.
		select {
		case <-stderrDone:
		case <-time.After(time.Second):
		}
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		<-reaped
		return fmt.Errorf("神通 JDBC 握手失败，请确认 %q 是 Java 8 或更高版本且能加载神通驱动: %w%s", javaBin, err, diagnostic)
	}
	slog.Info("Oscar JDBC bridge started", "driver", "com.oscar.Driver", "version", driverVersion)
	return nil
}

func decodeDriverVersion(body []byte) (string, error) {
	d := newDecoder(body)
	driverClass, err := d.string()
	if err != nil || driverClass != "com.oscar.Driver" {
		return "", errors.New("Oscar JDBC bridge handshake returned invalid driver metadata")
	}
	driverVersion, err := d.string()
	if err != nil || strings.TrimSpace(driverVersion) == "" {
		return "", errors.New("Oscar JDBC bridge handshake did not return a driver version")
	}
	return driverVersion, nil
}

func readBridgeStderr(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	var detail strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// JVM option notices may include environment secrets.
		if line == "" || strings.Contains(line, "OPTIONS:") {
			continue
		}
		slog.Warn("Oscar JDBC bridge", "message", line)
		if detail.Len() < 2048 {
			detail.WriteString("\n")
			detail.WriteString(line[:min(len(line), 2048-detail.Len())])
		}
	}
	if scanner.Err() != nil {
		_, _ = io.Copy(io.Discard, r)
	}
	return detail.String()
}

func (m *Manager) readLoop(gen uint64, r io.Reader) {
	for {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			m.processDied(gen, err)
			return
		}
		if n == 0 || n > maxFrameSize {
			m.processDied(gen, fmt.Errorf("invalid Oscar bridge response frame size %d", n))
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			m.processDied(gen, err)
			return
		}
		d := newDecoder(payload)
		version, err := d.byte()
		if err != nil || version != protocolVersion {
			m.processDied(gen, fmt.Errorf("Oscar bridge protocol version mismatch (Go v%d, JAR v%d); 请同时更新 dbgold 程序与 dbgold-oscar-bridge.jar", protocolVersion, version))
			return
		}
		marker, _ := d.byte()
		if marker != responseMarker {
			m.processDied(gen, fmt.Errorf("invalid Oscar bridge response marker"))
			return
		}
		requestID, err := d.uint64()
		if err != nil {
			m.processDied(gen, err)
			return
		}
		status, err := d.byte()
		if err != nil {
			m.processDied(gen, err)
			return
		}
		resp := response{}
		if status != 0 {
			state, _ := d.string()
			code, _ := d.int32()
			message, msgErr := d.string()
			if msgErr != nil {
				message = "malformed error response from Oscar JDBC bridge"
			}
			resp.err = &BridgeError{SQLState: state, Code: code, Message: message}
		} else {
			resp.body = make([]byte, d.Len())
			_, _ = io.ReadFull(d.Reader, resp.body)
		}
		m.pendingMu.Lock()
		ch := m.pending[requestID]
		delete(m.pending, requestID)
		m.pendingMu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (m *Manager) processDied(gen uint64, cause error) {
	m.processMu.Lock()
	if gen != m.generation || m.cmd == nil {
		m.processMu.Unlock()
		return
	}
	m.cmd, m.stdin, m.stdout = nil, nil, nil
	m.failPending(fmt.Errorf("Oscar JDBC bridge unavailable: %w", cause))
	m.processMu.Unlock()
}

func (m *Manager) failPending(err error) {
	m.pendingMu.Lock()
	pending := m.pending
	m.pending = make(map[uint64]chan response)
	m.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- response{err: err}
	}
}

func (m *Manager) writeRequest(requestID uint64, op byte, body []byte) error {
	e := &encoder{}
	e.byte(protocolVersion)
	e.byte(op)
	e.uint64(requestID)
	_, _ = e.Write(body)
	if e.Len() > maxFrameSize {
		return fmt.Errorf("Oscar bridge request is too large: %d bytes", e.Len())
	}
	m.processMu.Lock()
	w := m.stdin
	m.processMu.Unlock()
	if w == nil {
		return errors.New("Oscar JDBC bridge is not running")
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if err := binary.Write(w, binary.BigEndian, uint32(e.Len())); err != nil {
		return err
	}
	_, err := w.Write(e.Bytes())
	return err
}

func (m *Manager) request(ctx context.Context, op byte, body []byte) ([]byte, error) {
	if err := m.ensureStarted(ctx); err != nil {
		return nil, err
	}
	return m.requestStarted(ctx, op, body)
}

// requestExisting never starts a replacement JVM. This is used by operations
// bound to an existing session so a bridge crash cannot turn an old session ID
// into an implicit retry against a fresh process.
func (m *Manager) requestExisting(ctx context.Context, op byte, body []byte) ([]byte, error) {
	m.processMu.Lock()
	running := m.cmd != nil && m.cmd.Process != nil
	m.processMu.Unlock()
	if !running {
		return nil, errors.New("Oscar JDBC bridge is not running; open a new session to restart it")
	}
	return m.requestStarted(ctx, op, body)
}

func (m *Manager) requestStarted(ctx context.Context, op byte, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := m.nextID.Add(1)
	ch := make(chan response, 1)
	m.pendingMu.Lock()
	m.pending[id] = ch
	m.pendingMu.Unlock()
	if err := m.writeRequest(id, op, body); err != nil {
		m.pendingMu.Lock()
		delete(m.pending, id)
		m.pendingMu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp.body, resp.err
	case <-ctx.Done():
		m.pendingMu.Lock()
		delete(m.pending, id)
		m.pendingMu.Unlock()
		cancel := &encoder{}
		cancel.uint64(id)
		_ = m.writeRequest(m.nextID.Add(1), opCancel, cancel.Bytes())
		return nil, ctx.Err()
	}
}

func (m *Manager) OpenSession(ctx context.Context, opts SessionOptions) (uint64, error) {
	e := &encoder{}
	e.string(opts.URL)
	e.string(opts.Username)
	e.string(opts.Password)
	e.string(opts.Schema)
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 50
	}
	maxIdle := opts.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 25
	}
	lifetime := opts.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	e.int32(int32(maxOpen))
	e.int32(int32(maxIdle))
	e.int64(lifetime.Milliseconds())
	e.int32(int32(m.cfg.LoginTimeoutMS))
	e.int32(int32(m.cfg.SocketTimeoutMS))
	body, err := m.request(ctx, opOpenSession, e.Bytes())
	if err != nil {
		return 0, err
	}
	return newDecoder(body).uint64()
}

func sessionBody(session uint64) []byte {
	e := &encoder{}
	e.uint64(session)
	return e.Bytes()
}

func (m *Manager) Ping(ctx context.Context, session uint64) error {
	_, err := m.requestExisting(ctx, opPing, sessionBody(session))
	return err
}

func (m *Manager) ListSchemas(ctx context.Context, session uint64) ([]string, error) {
	body, err := m.requestExisting(ctx, opListSchemas, sessionBody(session))
	if err != nil {
		return nil, err
	}
	d := newDecoder(body)
	n, err := d.uint32()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		s, err := d.string()
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	sort.Strings(result)
	return result, nil
}

func (m *Manager) SchemaExists(ctx context.Context, session uint64, schema string) (bool, error) {
	list, err := m.ListSchemas(ctx, session)
	if err != nil {
		return false, err
	}
	for _, item := range list {
		if item == schema {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) Exec(ctx context.Context, session uint64, sql string) error {
	e := &encoder{}
	e.uint64(session)
	e.string(sql)
	_, err := m.requestExisting(ctx, opExec, e.Bytes())
	return err
}

func (m *Manager) QueryInt64(ctx context.Context, session uint64, sql string) (int64, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(sql)
	body, err := m.requestExisting(ctx, opQueryInt64, e.Bytes())
	if err != nil {
		return 0, err
	}
	return newDecoder(body).int64()
}

func (m *Manager) BeginBatch(ctx context.Context, session uint64, sql string) (uint64, error) {
	e := &encoder{}
	e.uint64(session)
	e.string(sql)
	body, err := m.requestExisting(ctx, opBatchBegin, e.Bytes())
	if err != nil {
		return 0, err
	}
	return newDecoder(body).uint64()
}

func (m *Manager) addBatchRows(ctx context.Context, batch uint64, rows [][]any) error {
	encodedRows, err := encodeRows(rows)
	if err != nil {
		return err
	}
	e := &encoder{}
	e.uint64(batch)
	_, _ = e.Write(encodedRows)
	_, err = m.requestExisting(ctx, opBatchAdd, e.Bytes())
	return err
}

func (m *Manager) CommitBatch(ctx context.Context, batch uint64) error {
	_, err := m.requestExisting(ctx, opBatchCommit, sessionBody(batch))
	return err
}

func (m *Manager) AbortBatch(ctx context.Context, batch uint64) error {
	_, err := m.requestExisting(ctx, opBatchAbort, sessionBody(batch))
	return err
}

func (m *Manager) CloseSession(ctx context.Context, session uint64) error {
	_, err := m.requestExisting(ctx, opCloseSession, sessionBody(session))
	return err
}

func (m *Manager) stopProcess(gen uint64) error {
	m.processMu.Lock()
	if gen != m.generation || m.cmd == nil {
		m.processMu.Unlock()
		return nil
	}
	cmd := m.cmd
	m.processMu.Unlock()
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func (m *Manager) Close() error {
	m.processMu.Lock()
	cmd := m.cmd
	gen := m.generation
	m.processMu.Unlock()
	if cmd == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := m.requestStarted(ctx, opShutdown, nil)
	if err != nil {
		_ = m.stopProcess(gen)
	}
	return err
}
