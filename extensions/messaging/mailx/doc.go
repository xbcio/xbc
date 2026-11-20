// Package mailx provides a configured XBC plugin whose primary value owns one
// SMTP client. Each named section below plugins.mail constructs one client, so
// a deployment can keep separate identities for transactional, alerting, or
// tenant-specific mail. Importing this package is side-effect free. Prefer
// explicit composition with mailx.Bundle(); executables that deliberately use
// process-wide autoload may blank-import the autoload subpackage.
//
// # Usage
//
// A consumer resolves the minimal Sender contract when a test double should be
// substitutable, or the plugin's concrete *Client when it already depends on
// this package:
//
//	var alerts = plugin.RefToInstance[mailx.Sender](mailx.Key, "alerts")
//
// plugin.RefTo[Sender](mailx.Key) is the same reference for the instance named
// "default".
//
//	type notifier struct {
//		sender mailx.Sender
//	}
//
//	var notifierDefinition = plugin.Define(
//		"notifier",
//		func(ctx plugin.BuildContext) (*notifier, error) {
//			return &notifier{sender: alerts.Get(ctx).Value}, nil
//		},
//		plugin.Options[*notifier]{Inputs: plugin.Inputs(alerts)},
//	)
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(notifierDefinition) }
//
// Sending constructs the message with go-mail, the same library type the
// plugin depends on:
//
//	msg := gomail.NewMsg()
//	_ = msg.From("noreply@example.com")
//	_ = msg.To("oncall@example.com")
//	msg.Subject("Nightly report")
//	msg.SetBodyString(gomail.TypeTextPlain, "All checks passed.")
//	err := alerts.Get(ctx).Value.DialAndSend(msg)
//
// The composition root includes both mailx.Bundle() and the consumer's Bundle.
// The integration owns the go-mail client. By default it verifies connectivity
// during construction -- one dial that also exercises EHLO, TLS, and AUTH --
// then closes that probe connection; every send opens and closes its own
// connection. Disabling verify deliberately makes construction lazy. XBC closes
// the client through a typed, idempotent lifecycle adapter, and a client that
// was stopped rejects further sends with ErrClosed.
//
// # Direct construction
//
// A host that owns the sender without an XBC application can construct the same
// value directly: New validates the configuration, applies the same TLS policy
// and construction-time verification, and returns a Sender labeled with the
// default instance. The result is released through Closer, which Client
// implements and which a caller reaches by type assertion because Sender stays
// the minimal send contract:
//
//	sender, err := mailx.New(cfg)
//	if err != nil {
//		return err
//	}
//	if closer, ok := sender.(mailx.Closer); ok {
//		defer func() { _ = closer.Close() }()
//	}
//
// New runs prepareConfig, so the TLS policy and authentication type are
// canonicalized and the same cross-field rules apply, but it does not apply the
// binder's field defaults: a directly constructed Config must name its port,
// where a plugin section defaults it to 25, and verify stays false unless set,
// where a plugin section defaults it to true. Close is idempotent and safe on a
// nil client, and a closed sender rejects sends with ErrClosed. Do not run one
// SMTP identity through both a plugin instance and New in the same process --
// two clients are difficult to attribute when delivery behavior differs.
//
// # Configuration
//
//	plugins:
//	  mail:
//	    alerts:
//	      host: "smtp.example.com"
//	      port: 587
//	      username: "svc-alerts"
//	      password: "${MAIL_PASSWORD}"   # supply through a secret source
//	      auth_type: "plain"             # empty means autodiscover
//	      tls:
//	        policy: "mandatory"          # mandatory | opportunistic | none
//	      timeout: "30s"
//	      connection_timeout: "5s"
//	      verify: true
//	    bulk:
//	      host: "smtp-bulk.example.com"
//	      port: 465
//	      username: "svc-bulk"
//	      password: "${MAIL_BULK_PASSWORD}"
//	      auth_type: "login"
//	      ssl:
//	        enable: true
//	        port: 465
//	      verify: false
//
// Empty auth_type selects go-mail's autodiscover, matching the legacy behavior.
// auth_type "custom" is rejected because it requires an application-supplied
// mechanism, which this plugin does not accept.
//
// # Legacy mapping
//
// An existing deployment described by a map of mail.<name> sections maps
// one-for-one:
//
//	legacy mail.<name>                this plugin
//	-------------------------------   ------------------------------------------
//	<map key>, e.g. "alerts"          the instance name, e.g. plugins.mail.alerts
//	host                              host
//	port                              port (default 25, the library default)
//	username                          username
//	password                          password
//	smtp.auth_type                    auth_type (empty selects autodiscover)
//	ssl.enable                        ssl.enable
//	ssl.port                          ssl.port
//	timeout (integer seconds)         timeout (Go duration, e.g. "30s")
//	connection_timeout (seconds)      connection_timeout (Go duration)
//	-                                 tls.policy (default mandatory)
//	-                                 verify (default true)
//
// The legacy default: true marker becomes the instance named "default", which
// is what plugin.RefTo resolves. The legacy implementation kept its
// verification connection open for later sends; this plugin closes it after
// verification and dials per send, because go-mail's send path never reused
// that connection anyway.
//
// # Secrets and TLS
//
// Password is a secret: the configuration view masks it, and the plugin never
// includes it in errors, logs, or formatted output. Supply it from XBC's secret
// sources rather than committing it.
//
// ssl.enable selects implicit TLS (the connection is encrypted from the first
// byte, typically port 465). ssl.port overrides port when both are set. With
// neither set, the legacy behavior connects to port 25 over implicit TLS, so a
// deployment that enables ssl should set ssl.port: 465 explicitly.
//
// For plain SMTP, tls.policy selects STARTTLS behavior. The default, mandatory,
// is the library default and fails construction when the server does not offer
// STARTTLS. opportunistic upgrades when offered; none disables STARTTLS and
// exists for local test servers only. The plugin never downgrades the default:
// a deployment must opt out explicitly.
//
// # Readiness
//
// The primary Client exports health.Contributor and contributes a readiness
// check named after the producing identity, "mail" or "mail[<instance>]". The
// check is contributed only when verify is enabled, and it tests TCP
// reachability without an SMTP or AUTH exchange, so a probe interval can never
// attempt a login or trip an account lockout. The contribution is inert unless
// the application also selects the health capability Bundle.
package mailx
