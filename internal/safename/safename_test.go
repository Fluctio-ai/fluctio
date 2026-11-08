package safename

import "testing"

// Windows reserves device names in ANY directory — and strips trailing
// dots/spaces, making "file." collide with "file". Sanitize must reject
// both classes rather than emit names the host FS mangles.
func TestSanitizeWindowsReserved(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"CON", ""},
		{"con.txt", ""},
		{"NUL.dat", ""},
		{"com1", ""},
		{"LPT9.log", ""},
		{"report.", "report"},
		{"notes  ", "notes"},
		// Reserved check is on the STEM: "constitution.md" is fine even
		// though it starts with "con".
		{"constitution.md", "constitution.md"},
		{"design notes v2.md", "design notes v2.md"},
	}
	for _, c := range cases {
		got := SanitizeFileName(c.in, 0)
		if got != c.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Regression: the pre-existing rules still hold.
func TestSanitizeBasics(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"..", ""},
		{"/etc/passwd", "passwd"},
		{"a/b/c.txt", "c.txt"},
		{"..\\..\\x.md", "x.md"},
		{"naïve 文件.md", "naïve 文件.md"},
	}
	for _, c := range cases {
		got := SanitizeFileName(c.in, 0)
		if got != c.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
