package prelude

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/recovery"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
)

func TestBundleCombinesExactlyTheCanonicalWebBaseline(t *testing.T) {
	want := plugin.CombineBundles(
		web.Bundle(),
		recovery.Bundle(),
		requestid.Bundle(),
		accesslog.Bundle(),
		securityheaders.Bundle(),
		gzip.Bundle(),
		timeout.Bundle(),
		health.Bundle(),
	)
	got := Bundle()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("Bundle() did not preserve the canonical Web baseline")
	}
	if !reflect.DeepEqual(got, Bundle()) {
		t.Fatal("Bundle() returned unstable composition content")
	}
}
