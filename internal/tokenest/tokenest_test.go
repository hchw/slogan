package tokenest

import "testing"

type fixed int

func (f fixed) Estimate([]byte) (int, error) { return int(f), nil }

func TestRegistryPrefersModelTokenizerAndFallsBack(t *testing.T) {
	r := NewRegistry(Conservative{MaxBytes: 100})
	r.Register("vendor/model", fixed(7))
	n, specific, err := r.Estimate("vendor/model", []byte("hello"))
	if err != nil || n != 7 || !specific {
		t.Fatalf("model estimate=%d specific=%v err=%v", n, specific, err)
	}
	n, specific, err = r.Estimate("other", []byte("hello"))
	if err != nil || n == 0 || specific {
		t.Fatalf("fallback=%d specific=%v err=%v", n, specific, err)
	}
}

func TestConservativeBoundsAndUnicode(t *testing.T) {
	e := Conservative{MaxBytes: 6}
	if _, err := e.Estimate([]byte("1234567")); err == nil {
		t.Fatal("expected byte cap")
	}
	a, _ := e.Estimate([]byte("abc"))
	b, _ := e.Estimate([]byte("中文"))
	if b < a {
		t.Fatalf("CJK estimate should account for weighted runes: ascii=%d cjk=%d", a, b)
	}
}

func TestRegistryRejectsNegativeEstimator(t *testing.T) {
	r := NewRegistry(Conservative{MaxBytes: 10})
	r.Register("bad", fixed(-1))
	if _, _, err := r.Estimate("bad", []byte("x")); err == nil {
		t.Fatal("expected negative count error")
	}
}
