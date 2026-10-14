package repohost

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChangeTitle(t *testing.T) {
	long := strings.Repeat("Work item `split` (milestone m1). ", 20) // ~680 chars
	cases := []struct{ in, want string }{
		{"Add subtract()", "Add subtract()"},
		{"  Work item `split`.\nReservation: lib/domain/split.dart\n", "Work item `split`."},
		{"\n\n", "t123"},
		{"", "t123"},
	}
	for _, c := range cases {
		if got := ChangeTitle(c.in, "t123"); got != c.want {
			t.Errorf("ChangeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := ChangeTitle(long, "t123")
	if n := utf8.RuneCountInString(got); n != MaxTitleLen || !strings.HasSuffix(got, "…") {
		t.Errorf("long title: %d runes, suffix %q", n, got[len(got)-3:])
	}
	// A multibyte title is cut on a rune boundary.
	if got := ChangeTitle(strings.Repeat("é", 300), "x"); !utf8.ValidString(got) || utf8.RuneCountInString(got) != MaxTitleLen {
		t.Errorf("multibyte cut: valid=%v runes=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
	}
}
