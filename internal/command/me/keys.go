package me

// The Data keys this package's outcomes carry.
//
// A key names a fact, not a category: the status says which branch
// the work took. A key with no value does not appear at all.
const (
	// KeyIterations is how many rounds the run was asked to make.
	KeyIterations = "iterations"
	// KeyRunID is the id of the run.
	KeyRunID = "run_id"
	// KeyStep is the id of the step the command acted on.
	KeyStep = "step"
	// KeyWorkflow is the name of the workflow.
	KeyWorkflow = "workflow"
)
