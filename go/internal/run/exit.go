package run

// ExitFinal is the exit code with which fetch and the runner report a failure
// that another attempt cannot fix, such as a PR head that moved on or a result
// the controller rejected. The controller retries any other failure
// (docs/architecture.md §4.3). runner/src/main.ts mirrors it.
const ExitFinal = 3
