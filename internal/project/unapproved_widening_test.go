package project

import (
	"slices"
	"testing"
)

func TestUnapprovedWidening(t *testing.T) {
	cases := []struct{ now, approved, want []string }{
		{nil, nil, nil},
		{[]string{"access"}, nil, []string{"access"}},
		{[]string{"access"}, []string{"access", "mounts"}, nil},
		{[]string{"access", "path"}, []string{"access"}, []string{"path"}},
	}
	for _, c := range cases {
		if got := UnapprovedWidening(c.now, c.approved); !slices.Equal(got, c.want) {
			t.Errorf("UnapprovedWidening(%v, %v) = %v, want %v", c.now, c.approved, got, c.want)
		}
	}
}
