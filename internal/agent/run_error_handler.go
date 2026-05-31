package agent

// RunErrorAction specifies what to do after an error.
type RunErrorAction int

const (
	ErrorActionRetry RunErrorAction = iota
	ErrorActionAbort
	ErrorActionContinue
)

// RunErrorData provides context about a run-level error.
type RunErrorData struct {
	Error error
	Agent *Agent
	Turn  int
}

// RunErrorHandlerResult is the decision from an error handler.
type RunErrorHandlerResult struct {
	Action  RunErrorAction
	Message string
}

// RunErrorHandler decides what to do when a run encounters an error.
type RunErrorHandler func(RunErrorData) RunErrorHandlerResult
