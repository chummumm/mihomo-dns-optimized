package log

// DNSDebugEnabled is used only by the fork's hot DNS path. Any subscriber keeps
// debug events enabled even when the console is silent.
func DNSDebugEnabled() bool { return Level() <= DEBUG || source.HasSubscribers() }
