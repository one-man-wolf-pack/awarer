package process_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/process"
)

const (
	probeRoleEnv  = "AWA_PROCESS_PROBE_ROLE"
	probeHelperRe = "^TestProbeHelperProcess$"
)

func TestProbeHelperProcess(t *testing.T) {
	role := os.Getenv(probeRoleEnv)
	switch role {
	case "":
		t.Skip("helper process for the probe cases")
	case "report":
		wd, _ := os.Getwd()
		in, _ := io.ReadAll(os.Stdin)
		fmt.Printf("args=%q cwd=%s stdin=%d env=%q\n", os.Args[2:], filepath.Base(wd), len(in), os.Environ())
	case "exact":
		os.Stdout.WriteString(os.Getenv("AWA_PROBE_TEXT"))
	case "fail":
		fmt.Println("version 1")
		os.Exit(3)
	case "empty":
		fmt.Fprintln(os.Stderr, "only diagnostics")
	case "sleep":
		time.Sleep(30 * time.Second)
	case "flood-stdout":
		os.Stdout.Write(make([]byte, 1<<20))
	case "flood-stderr":
		fmt.Println("version 1")
		os.Stderr.Write(make([]byte, 1<<20))
	case "hold-pipe":
		child := exec.Command(os.Args[0], "-test.run="+probeHelperRe)
		child.Env = append(os.Environ(), probeRoleEnv+"=sleep-short")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		fmt.Println("version 1")
	case "flood-descendant-stdout", "flood-descendant-stderr":
		child := exec.Command(os.Args[0], "-test.run="+probeHelperRe)
		child.Env = append(os.Environ(), probeRoleEnv+"=flood-"+strings.TrimPrefix(role, "flood-descendant-")+"-counted")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		fmt.Println("version 1")
		time.Sleep(5 * time.Second)
	case "flood-stdout-counted", "flood-stderr-counted":
		signal.Ignore(syscall.SIGPIPE)
		out := os.Stdout
		if role == "flood-stderr-counted" {
			out = os.Stderr
		}
		chunk := make([]byte, 32<<10)
		total := 0
		for range 4096 {
			n, err := out.Write(chunk)
			total += n
			if err != nil {
				break
			}
		}
		_ = os.WriteFile(os.Getenv("AWA_PROBE_COUNT"), []byte(strconv.Itoa(total)), 0o600)
	case "sleep-short":
		time.Sleep(5 * time.Second)
	case "kill-self":
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Kill()
		time.Sleep(time.Second)
	}
	os.Exit(0)
}

func probeSpec(t *testing.T, role string, env ...string) runcache.ProbeSpec {
	t.Helper()
	return runcache.ProbeSpec{
		Argv:      []string{os.Args[0], "-test.run=" + probeHelperRe},
		Dir:       t.TempDir(),
		Env:       append([]string{probeRoleEnv + "=" + role}, env...),
		Timeout:   10 * time.Second,
		MaxStdout: 64 << 10,
		MaxStderr: 64 << 10,
	}
}

func TestProbeRunsLiteralArgvInDirWithOnlyTheGivenEnvAndNullStdin(t *testing.T) {
	spec := probeSpec(t, "report", "KEPT=1")
	spec.Argv = append(spec.Argv, "a b", "$HOME")
	res := process.New().Probe(context.Background(), spec)
	if res.Status != runcache.FactAcquired {
		t.Fatalf("status = %s", res.Status)
	}
	got := string(res.Stdout)
	for _, want := range []string{`args=["a b" "$HOME"]`, "cwd=" + filepath.Base(spec.Dir), "stdin=0", `env=["` + probeRoleEnv + `=report" "KEPT=1"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("report %q lacks %q", got, want)
		}
	}
}

func TestProbeReturnsExactStdoutBytes(t *testing.T) {
	for _, text := range []string{"v1", "v1\n", " v1 \r\n\n"} {
		res := process.New().Probe(context.Background(), probeSpec(t, "exact", "AWA_PROBE_TEXT="+text))
		if res.Status != runcache.FactAcquired || string(res.Stdout) != text {
			t.Errorf("text %q: status %s stdout %q", text, res.Status, res.Stdout)
		}
	}
}

func TestProbeFailuresAreTypedUnavailability(t *testing.T) {
	cases := []struct {
		role string
		want runcache.FactStatus
	}{
		{"fail", runcache.FactExitNonzero},
		{"empty", runcache.FactEmptyStdout},
		{"flood-stdout", runcache.FactStdoutOverflow},
		{"flood-stderr", runcache.FactStderrOverflow},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			role string
			want runcache.FactStatus
		}{"kill-self", runcache.FactSignaled})
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			res := process.New().Probe(context.Background(), probeSpec(t, tc.role))
			if res.Status != tc.want || res.Stdout != nil {
				t.Fatalf("status = %s stdout %q, want %s and no bytes", res.Status, res.Stdout, tc.want)
			}
		})
	}
	spec := probeSpec(t, "report")
	spec.Argv = []string{"awa-no-such-probe"}
	spec.Env = []string{"PATH=" + t.TempDir()}
	if res := process.New().Probe(context.Background(), spec); res.Status != runcache.FactMissingExecutable {
		t.Errorf("missing executable: status = %s", res.Status)
	}
	spec = probeSpec(t, "report")
	notAProgram := filepath.Join(t.TempDir(), "not-a-program.exe")
	if err := os.WriteFile(notAProgram, []byte{0, 1, 2, 3}, 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	spec.Argv = []string{notAProgram}
	if res := process.New().Probe(context.Background(), spec); res.Status != runcache.FactStartFailed {
		t.Errorf("start failure: status = %s", res.Status)
	}
}

func TestProbeTimeoutEndsTheProcess(t *testing.T) {
	spec := probeSpec(t, "sleep")
	spec.Timeout = 300 * time.Millisecond
	start := time.Now()
	res := process.New().Probe(context.Background(), spec)
	if res.Status != runcache.FactTimedOut {
		t.Fatalf("status = %s", res.Status)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timed-out probe returned after %s", d)
	}
}

func TestProbeIsBoundedWhileADescendantHoldsItsPipe(t *testing.T) {
	start := time.Now()
	res := process.New().Probe(context.Background(), probeSpec(t, "hold-pipe"))
	if res.Status != runcache.FactTimedOut {
		t.Fatalf("status = %s, want timed-out: the output never completed", res.Status)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("probe waited %s for a pipe its descendant held open", d)
	}
}

func TestProbeStopsOnParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	res := process.New().Probe(ctx, probeSpec(t, "sleep"))
	if res.Status == runcache.FactAcquired {
		t.Fatal("a cancelled probe was acquired")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancelled probe returned after %s", d)
	}
}

func TestProbeStopsConsumingAFloodingDescendantAtItsBound(t *testing.T) {
	for _, tc := range []struct {
		stream string
		want   runcache.FactStatus
	}{
		{"stdout", runcache.FactStdoutOverflow},
		{"stderr", runcache.FactStderrOverflow},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			count := filepath.Join(t.TempDir(), "count")
			spec := probeSpec(t, "flood-descendant-"+tc.stream, "AWA_PROBE_COUNT="+count)
			spec.MaxStdout, spec.MaxStderr = 1024, 1024
			res := process.New().Probe(context.Background(), spec)
			if res.Status != tc.want || res.Stdout != nil {
				t.Fatalf("status = %s stdout %d bytes, want %s without output", res.Status, len(res.Stdout), tc.want)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				b, err := os.ReadFile(count) //nolint:gosec // test fixture path
				if err == nil {
					n, err := strconv.Atoi(string(b))
					if err != nil {
						t.Fatal(err)
					}
					if n > 128<<10 {
						t.Fatalf("the probe consumed %d bytes for a 1024-byte bound", n)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the flooding descendant never saw its pipe close")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
