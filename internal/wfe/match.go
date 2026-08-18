package wfe

// Match reports whether ev should fire wf. Pure function. The
// caller (bot's trigger pipeline) is responsible for workspace
// filtering; Match only checks the on: spec.
func Match(wf *Workflow, ev Event) bool {
	switch ev.Kind {
	case "schedule":
		return matchSchedule(wf, ev)
	case "pull_request":
		return matchPR(wf, ev)
	case "branch":
		return matchBranch(wf, ev)
	case "issue":
		return matchIssue(wf, ev)
	case "mention":
		return matchMention(wf, ev)
	}
	return false
}

func matchSchedule(wf *Workflow, ev Event) bool {
	if wf.On.Schedule == nil {
		return false
	}
	cron, _ := ev.Data["cron"].(string)
	if cron == "" {
		return false
	}
	for _, s := range wf.On.Schedule {
		if s.Cron == cron {
			return true
		}
	}
	return false
}

func matchPR(wf *Workflow, ev Event) bool {
	if wf.On.PullRequest == nil {
		return false
	}
	branch, _ := ev.Data["branch"].(string)
	action, _ := ev.Data["action"].(string)
	if wf.On.PullRequest.Branches != nil && !contains(wf.On.PullRequest.Branches, branch) {
		return false
	}
	if wf.On.PullRequest.Events != nil && !contains(wf.On.PullRequest.Events, action) {
		return false
	}
	return true
}

func matchBranch(wf *Workflow, ev Event) bool {
	if wf.On.Branch == nil {
		return false
	}
	branch, _ := ev.Data["name"].(string)
	action, _ := ev.Data["action"].(string)
	if wf.On.Branch.Patterns != nil && !anyMatch(wf.On.Branch.Patterns, branch) {
		return false
	}
	if wf.On.Branch.Events != nil && !contains(wf.On.Branch.Events, action) {
		return false
	}
	return true
}

func matchIssue(wf *Workflow, ev Event) bool {
	if wf.On.Issue == nil {
		return false
	}
	action, _ := ev.Data["action"].(string)
	if wf.On.Issue.Events != nil && !contains(wf.On.Issue.Events, action) {
		return false
	}
	return true
}

func matchMention(wf *Workflow, ev Event) bool {
	if wf.On.Mention == nil {
		return false
	}
	// No commands whitelist = any mention fires.
	if len(wf.On.Mention.Commands) == 0 {
		return true
	}
	cmd, _ := ev.Data["command"].(string)
	return contains(wf.On.Mention.Commands, cmd)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// anyMatch reports whether v matches any of the glob-style
// patterns. "*" matches everything. "release/*" matches strings
// starting with "release/". (Simplified glob — not full Git
// wildcard syntax. The trigger pipeline can replace with filepath.Match
// if richer matching is needed.)
func anyMatch(patterns []string, v string) bool {
	for _, p := range patterns {
		if matchGlob(p, v) {
			return true
		}
	}
	return false
}

func matchGlob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if i := indexByte(pattern, '*'); i >= 0 {
		prefix := pattern[:i]
		suffix := pattern[i+1:]
		if len(s) < len(prefix)+len(suffix) {
			return false
		}
		return s[:len(prefix)] == prefix && s[len(s)-len(suffix):] == suffix
	}
	return pattern == s
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
