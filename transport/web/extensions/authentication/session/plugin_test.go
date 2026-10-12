package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

// TestBundleSelectsTheNeutralSessionPlugin pins the production spelling: a
// composition root that selects session.Bundle() from this package gets the
// neutral session plugin too, so it never has to know the plugin was split.
func TestBundleSelectsTheNeutralSessionPlugin(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(sessioncore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral session plugin then the Web adapter", got)
	}
}

// TestDefinitionExportsTheExtractorAndTheCookieManager keeps the authenticator
// contract on the neutral half. Exporting it here as well would register the
// session scheme twice, which the Web authentication middleware refuses.
func TestDefinitionExportsTheExtractorAndTheCookieManager(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := []reflect.Type{
		reflect.TypeOf((*web.CredentialExtractor)(nil)).Elem(),
		reflect.TypeOf((*Manager)(nil)).Elem(),
	}
	var got []reflect.Type
	for _, contract := range descriptor.Contracts {
		got = append(got, contract.Type)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Definition contracts = %v, want %v", got, want)
	}
}

func TestNewRequiresTheSessionPlugin(t *testing.T) {
	if p, err := New(nil); err == nil || p != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil adapter and error", p, err)
	}
}

func TestSetCookieAppliesTheConfiguredAttributesAndClearCookieExpiresIt(t *testing.T) {
	p, _ := configuredSessionPlugin(t, func(cfg *sessioncore.Config) {
		cfg.Name = "__Host-session"
		cfg.SameSite = "strict"
	})
	value, err := p.Create(context.Background(), "sensitive-subject", nil)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ctx := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if err := p.SetCookie(ctx, value); err != nil {
		t.Fatal(err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-session" || cookie.Value != value.ID || cookie.Path != "/" || cookie.Domain != "" ||
		!cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %#v", cookie)
	}
	// The default ttl is 24h; MaxAge rounds the remaining lifetime up.
	if cookie.MaxAge < int((24*time.Hour-time.Minute)/time.Second) || cookie.MaxAge > int((24*time.Hour)/time.Second) {
		t.Fatalf("cookie MaxAge = %d, want about 24h", cookie.MaxAge)
	}
	if strings.Contains(recorder.Header().Get("Set-Cookie"), value.Subject) {
		t.Fatal("cookie leaked cleartext identity")
	}

	cleared := httptest.NewRecorder()
	p.ClearCookie(enginetest.NewCtx(cleared, httptest.NewRequest(http.MethodGet, "/", nil)))
	clearCookies := cleared.Result().Cookies()
	if len(clearCookies) != 1 || clearCookies[0].Name != "__Host-session" || clearCookies[0].Value != "" ||
		clearCookies[0].MaxAge != -1 || !clearCookies[0].HttpOnly || !clearCookies[0].Secure {
		t.Fatalf("clear cookie = %#v", clearCookies)
	}
}

func TestSetCookieRefusesInvalidSessionsNilContextsAndUseAfterStop(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	value, err := p.Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	malformed := value
	malformed.ID = "attacker-selected"
	if err := p.SetCookie(ctx, malformed); !errors.Is(err, sessioncore.ErrInvalidCookie) {
		t.Fatalf("SetCookie(malformed) = %v", err)
	}
	expired := value
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if err := p.SetCookie(ctx, expired); !errors.Is(err, sessioncore.ErrInvalidCookie) {
		t.Fatalf("SetCookie(expired) = %v", err)
	}
	if err := p.SetCookie(nil, value); err == nil {
		t.Fatal("SetCookie(nil ctx) = nil error")
	}
	p.ClearCookie(nil)

	if err := core.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCookie(ctx, value); !errors.Is(err, sessioncore.ErrStopped) {
		t.Fatalf("SetCookie after stop = %v", err)
	}
}
