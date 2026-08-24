package procgroup

import "testing"

func TestTailBufferKeepsOnlyConfiguredTail(t *testing.T) {
	buf := NewTailBuffer(5)
	for _, chunk := range []string{"abc", "defg", "hijklmnop"} {
		if n, err := buf.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := buf.String(); got != "lmnop" {
		t.Fatalf("tail = %q, want %q", got, "lmnop")
	}
}

func TestTailBufferWithNonPositiveLimitDiscardsOutput(t *testing.T) {
	buf := NewTailBuffer(0)
	if n, err := buf.Write([]byte("discarded")); err != nil || n != len("discarded") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("tail = %q, want empty", got)
	}
}
