package runtime

// Run is the process entry point for the common case:
//
//	func main() { xbc.Run() }
//
// The canonical owner of process facilities -- os.Args, signal registration,
// diagnostics, logger flushing, and process exit -- is process.go. This entry
// point only constructs the App and delegates to that owner. Embedded callers
// should use App.Execute instead, which touches none of those.
func Run() {
	app, err := New()
	if err != nil {
		exitWithError(err)
		return
	}
	runProcess(app)
}
