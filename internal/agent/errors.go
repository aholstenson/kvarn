package agent

import "errors"

// ErrRefused marks a run that ended because the model would not continue the
// task: the model provider's content filter kept blocking it, or the model
// wrote a refusal. It is not a defect in the change or in kvarn, and another
// validation pass cannot clear it, so callers report it as its own reason
// rather than as a crash.
var ErrRefused = errors.New("the model refused to continue the task")
