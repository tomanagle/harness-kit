package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/internal/processtest"
	"github.com/tomanagle/harness-kit/core/runtimes/transport"
)

func TestLauncherChildProcess(t *testing.T) {
	if os.Getenv("HK_LAUNCHER_TEST_CHILD") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, "final protocol response\n")
	os.Exit(0)
}

func TestLauncherPreservesStdoutAfterWait(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child, err := (OSLauncher{}).Start(ctx, transport.Command{Path: executable, Args: []string{"-test.run=^TestLauncherChildProcess$"}, Env: []string{"HK_LAUNCHER_TEST_CHILD=1"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Successful startup is not a process-lifetime cancellation scope.
	defer child.Stdout().Close()
	_ = child.Stdin().Close()
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(child.Stdout())
	if err != nil || string(data) != "final protocol response\n" {
		t.Fatalf("final output was lost after Wait: %q %v", data, err)
	}
}

func TestVersionWithCancelsInjectedChild(t *testing.T) {
	launcher := &processtest.Launcher{Run: func(ctx context.Context, _ transport.Command, _ io.Reader, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := VersionWith(ctx, launcher, "virtual", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe cancellation: %v", err)
	}
}
