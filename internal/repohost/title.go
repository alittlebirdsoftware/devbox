package repohost

import "strings"

// MaxTitleLen is the longest change title both hosts accept (GitLab refuses
// 256 characters and more; GitHub 257 and more).
const MaxTitleLen = 255

// ChangeTitle derives the change's title from the task title: its first
// non-empty line, trimmed, cut to MaxTitleLen characters. A --task run's title
// is the whole task text, which can run to several lines and well past the
// hosts' limits; an --issue run's is the issue title. fallback (the task id)
// stands in for an empty title.
func ChangeTitle(title, fallback string) string {
	for _, line := range strings.Split(title, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > MaxTitleLen {
			line = strings.TrimSpace(string(r[:MaxTitleLen-1])) + "…"
		}
		return line
	}
	return fallback
}
