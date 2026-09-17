package runtime

// Run assembles an App from options and runs it as the whole process:
//
//	func main() { xbc.Run(xbc.WithBundles(orders.Bundle())) }
//
// It takes the same Options as New, so an explicit Bundle composition and the
// blank-import autoload composition reach the process facilities through the
// same entry point; passing no Option composes the frozen autoload catalog.
//
// The canonical owner of process facilities -- os.Args, signal registration,
// diagnostics, logger flushing, and process exit -- is process.go. This entry
// point only constructs the App and delegates to that owner. Callers that own
// the process themselves should use New and App.Execute instead, which touch
// none of those.
func Run(options ...Option) {
	app, err := New(options...)
	if err != nil {
		exitWithError(err)
		return
	}
	runProcess(app)
}
