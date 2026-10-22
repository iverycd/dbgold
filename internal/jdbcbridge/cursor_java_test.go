package jdbcbridge

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestJavaValidationCursorProtocol(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac is unavailable")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java is unavailable")
	}
	dir := t.TempDir()
	main := filepath.Join("..", "..", "jdbcbridge", "java", "src", "com", "dbgold", "oscar", "BridgeMain.java")
	if out, err := exec.Command(javac, "--release", "8", "-encoding", "UTF-8", "-d", dir, main, filepath.Join("testdata", "BridgeMainValidationTest.java")).CombinedOutput(); err != nil {
		t.Fatalf("compile Java cursor test: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", dir, "com.dbgold.oscar.BridgeMainValidationTest").CombinedOutput(); err != nil {
		t.Fatalf("Java cursor protocol: %v\n%s", err, out)
	}
}
