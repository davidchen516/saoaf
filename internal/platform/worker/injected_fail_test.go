package worker

import "testing"

func TestInjectedFailure(t *testing.T) {
	t.Fatal("red-run probe: failing unit test")
}
