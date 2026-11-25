package rules

import "testing"

// Known answers of XXH64 with seed 0, of lengths under 4, under 32 and
// past it.
func TestXXHash64(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint64
	}{
		{"", 0xef46db3751d8e999},
		{"a", 0xd24ec4f1a98c6e5b},
		{"abc", 0x44bc2cf5ad770999},
		{"Nobody inspects the spammish repetition", 0xfbcea83c8a378bf1},
	} {
		if got := xxhash64([]byte(c.in)); got != c.want {
			t.Errorf("%q: %#x, want %#x", c.in, got, c.want)
		}
	}
}
