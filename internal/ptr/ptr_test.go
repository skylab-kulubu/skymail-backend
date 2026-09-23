package ptr

import "testing"

func TestEqual(t *testing.T) {
	one, alsoOne, two := "bir", "bir", "iki"
	for _, tc := range []struct {
		a, b *string
		want bool
	}{
		{nil, nil, true},
		{&one, nil, false},
		{nil, &one, false},
		{&one, &alsoOne, true},
		{&one, &two, false},
	} {
		if got := Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
