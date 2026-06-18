package status

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCPUSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("cpu  10 20 30 40 5 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	sample, err := readCPUSample(path)
	if err != nil {
		t.Fatalf("readCPUSample() error = %v", err)
	}
	if sample.total != 105 || sample.idle != 45 {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestReadMemInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\n"), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	total, available, err := readMemInfo(path)
	if err != nil {
		t.Fatalf("readMemInfo() error = %v", err)
	}
	if total != 1000 || available != 250 {
		t.Fatalf("total=%d available=%d", total, available)
	}
}

func TestClampPercent(t *testing.T) {
	if got := clampPercent(-1); got != 0 {
		t.Fatalf("clampPercent(-1) = %v", got)
	}
	if got := clampPercent(101); got != 100 {
		t.Fatalf("clampPercent(101) = %v", got)
	}
	if got := clampPercent(42.5); got != 42.5 {
		t.Fatalf("clampPercent(42.5) = %v", got)
	}
}
