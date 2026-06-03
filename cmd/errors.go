package cmd

// exitCoder lets a command suggest a specific process exit code (D4: 0 success,
// 1 runtime error, 2 usage error).
type exitCoder interface {
	error
	ExitCode() int
}

// usageError is a bad-invocation error: exit code 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
func (e usageError) ExitCode() int { return 2 }

// quietExit carries a process exit code without an error message — for commands
// (like `audit --exit-code`) that completed successfully but want to signal a
// result via the exit status. Execute prints nothing for an empty message.
type quietExit struct{ code int }

func (e quietExit) Error() string { return "" }
func (e quietExit) ExitCode() int { return e.code }
