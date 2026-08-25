package log

import (
	"context"
	"io"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Benchmarks for the claims this package's comments make but never measured:
// hit()'s zero-allocation stack-buffer path, apply()'s copy-on-write, the
// cost the masking layer adds to a full entry, and whether the T-series
// sugar is really equivalent to Ctx(ctx).
//
// Everything writes to io.Discard so the numbers reflect encode+mask cost,
// not disk.

// discardLogger builds a Logger writing to io.Discard, optionally wrapped in
// maskCore, so the masking layer's cost can be isolated by difference.
func discardLogger(enc zapcore.Encoder, masked bool) Logger {
	var core zapcore.Core = zapcore.NewCore(enc, zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	if masked {
		core = newMaskCore(core, newMasker(nil))
	}
	return newZapLogger(zap.New(core))
}

// ── hit(): the genuine hot path, called per field per entry ──────────────

func BenchmarkMaskerHit(b *testing.B) {
	m := newMasker(nil)

	// longName exceeds maskKeyBufSize (64), forcing the hitSlow heap path.
	longName := strings.Repeat("verylongsegment_", 6) + "id" // 98 bytes
	// manyWords exceeds maskKeyMaxWords (12), also forcing hitSlow.
	manyWords := "a_b_c_d_e_f_g_h_i_j_k_l_m_n_id" // 15 words

	cases := []struct {
		name string
		key  string
	}{
		{"miss_short", "user_id"},        // most common case: no hit
		{"miss_camel", "orderCreatedAt"}, // camelCase splitting, no hit
		{"miss_near", "token_count"},     // head word is count -- must not hit
		{"hit_whole", "password"},        // whole-string hit
		{"hit_suffix", "db_password"},    // suffix word group hit
		{"hit_camel", "accessToken"},     // camelCase normalized hit
		{"overflow_buf", longName},       // > maskKeyBufSize -> hitSlow
		{"overflow_words", manyWords},    // > maskKeyMaxWords -> hitSlow
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sinkBool = m.hit(c.key)
			}
		})
	}
}

// sinkBool prevents the compiler from optimizing the hit call away.
var sinkBool bool

// ── apply(): the zero-allocation-when-nothing-hits claim ────────────────

func BenchmarkMaskerApply(b *testing.B) {
	m := newMasker(nil)

	clean := []zapcore.Field{
		zap.String("user", "alice"),
		zap.Int("order_id", 1001),
		zap.String("status", "paid"),
		zap.Float64("amount", 99.5),
	}
	dirty := []zapcore.Field{
		zap.String("user", "alice"),
		zap.String("password", "hunter2"),
		zap.Int("order_id", 1001),
		zap.String("status", "paid"),
	}

	b.Run("no_hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sinkFields = m.apply(clean)
		}
	})
	b.Run("one_hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sinkFields = m.apply(dirty)
		}
	})
}

var sinkFields []zapcore.Field

// ── Full entry: console vs json, and what masking costs ─────────────────

func BenchmarkEntry(b *testing.B) {
	cases := []struct {
		name   string
		enc    func() zapcore.Encoder
		masked bool
	}{
		{"console_nomask", func() zapcore.Encoder { return newConsoleEncoder(false) }, false},
		{"console_mask", func() zapcore.Encoder { return newConsoleEncoder(false) }, true},
		{"json_nomask", func() zapcore.Encoder { return zapcore.NewJSONEncoder(jsonEncoderConfig()) }, false},
		{"json_mask", func() zapcore.Encoder { return zapcore.NewJSONEncoder(jsonEncoderConfig()) }, true},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			l := discardLogger(c.enc(), c.masked)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				l.Info("订单已支付",
					"order_id", 1001,
					"user", "alice",
					"amount", 99.5,
					"status", "paid")
			}
		})
	}
}

// ── The reflect path: only zap.Any with a composite value reaches it ────

type benchOrder struct {
	ID       int    `json:"id"`
	User     string `json:"user"`
	Password string `json:"password"` // hits the blacklist -> forces the copy path
	Status   string `json:"status"`
}

func BenchmarkReflectPath(b *testing.B) {
	l := discardLogger(zapcore.NewJSONEncoder(jsonEncoderConfig()), true)
	v := benchOrder{ID: 1001, User: "alice", Password: "hunter2", Status: "paid"}

	b.Run("struct_with_hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			l.Info("下单", "order", v)
		}
	})

	clean := struct {
		ID   int    `json:"id"`
		User string `json:"user"`
	}{1001, "alice"}
	b.Run("struct_no_hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			l.Info("下单", "order", clean)
		}
	})
}

// ── T-series sugar vs Ctx(ctx): the two paths claim to be equivalent ────

func BenchmarkSugar(b *testing.B) {
	l := discardLogger(zapcore.NewJSONEncoder(jsonEncoderConfig()), true)
	ctx := NewContext(WithTrace(context.Background(), NewTrace("GET /orders")), l)

	b.Run("TInfo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			TInfo(ctx, "请求进入", "order_id", 1001)
		}
	})
	b.Run("CtxInfo", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Ctx(ctx).Info("请求进入", "order_id", 1001)
		}
	})
}

// ── Trace: per-request cost of starting and propagating a trace ─────────

func BenchmarkTrace(b *testing.B) {
	b.Run("NewTrace", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sinkTrace = NewTrace("GET /orders")
		}
	})

	base := NewTrace("GET /orders")
	b.Run("Fork", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sinkTrace = base.Fork("db.query")
		}
	})

	ctx := WithTrace(context.Background(), base)
	b.Run("TraceFrom", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			sinkTrace = TraceFrom(ctx)
		}
	})
}

var sinkTrace Trace
