package auditlog_test

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	auditlog "github.com/xbcio/xbc/transport/web/extensions/observability/auditlog"
)

func TestOrdinaryImportHasNoRegistrationSideEffect(t *testing.T) {
	var zero plugin.Definition
	if auditlog.Definition() == zero || auditlog.Definition() != auditlog.Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := auditlog.Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, auditlog.Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}
