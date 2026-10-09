package log

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Masking is decided once per value type: every typed Add*/Append* path either
// replaces the value on a blocked key or forwards the original. Those paths are
// the only way a caller writes a field without going through reflection, so
// each one is pinned individually here -- dropping a single hit() branch is
// otherwise an invisible regression whose only symptom is a leaked credential
// of exactly one Go type.
//
// The oracle is differential: the same field is written once through a masked
// encoder and once through an unmasked one. A blocked key must come out as the
// placeholder, and a clean key must come out exactly as the unmasked encoder
// wrote it -- so a branch that masks unconditionally fails just as loudly as
// one that stops masking.

// plainLogger is the unmasked counterpart of maskedLogger.
func plainLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

const probeSecret = "hunter2"

// valueWriter writes one value with a single typed ObjectEncoder method.
type valueWriter func(enc zapcore.ObjectEncoder, key string) error

// objectProbe writes the same value under the clean key and the blocked key.
type objectProbe struct{ write valueWriter }

func (p objectProbe) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if err := p.write(enc, "subject"); err != nil {
		return err
	}
	return p.write(enc, "password")
}

// probedObject logs one object under "probe" and returns it decoded.
func probedObject(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs, value zapcore.ObjectMarshaler) map[string]any {
	t.Helper()
	logger.Info("probe", zap.Object("probe", value))
	require.Len(t, logs.All(), 1)
	probe, ok := logs.All()[0].ContextMap()["probe"].(map[string]any)
	require.True(t, ok, "decoded probe actual %T", logs.All()[0].ContextMap()["probe"])
	return probe
}

// probeObjectValues are the values every type is written with, chosen so a
// dropped or mistyped forwarding is visible in the decoded output.
func probeObjectValues() []struct {
	name  string
	write valueWriter
} {
	return []struct {
		name  string
		write valueWriter
	}{
		{"AddArray", func(enc zapcore.ObjectEncoder, key string) error {
			return enc.AddArray(key, stringArray{values: []string{probeSecret}})
		}},
		{"AddObject", func(enc zapcore.ObjectEncoder, key string) error {
			return enc.AddObject(key, stringObject{value: probeSecret})
		}},
		{"AddReflected", func(enc zapcore.ObjectEncoder, key string) error {
			return enc.AddReflected(key, map[string]any{"inner": probeSecret})
		}},
		{"AddBinary", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddBinary(key, []byte(probeSecret))
			return nil
		}},
		{"AddByteString", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddByteString(key, []byte(probeSecret))
			return nil
		}},
		{"AddBool", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddBool(key, true)
			return nil
		}},
		{"AddComplex128", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddComplex128(key, complex(1, -2))
			return nil
		}},
		{"AddComplex64", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddComplex64(key, complex64(complex(1, -2)))
			return nil
		}},
		{"AddDuration", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddDuration(key, 1500*time.Millisecond)
			return nil
		}},
		{"AddFloat32", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddFloat32(key, 1.5)
			return nil
		}},
		{"AddFloat64", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddFloat64(key, -2.25)
			return nil
		}},
		{"AddInt", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddInt(key, -1)
			return nil
		}},
		{"AddInt64", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddInt64(key, -2)
			return nil
		}},
		{"AddInt32", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddInt32(key, -3)
			return nil
		}},
		{"AddInt16", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddInt16(key, -4)
			return nil
		}},
		{"AddInt8", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddInt8(key, -5)
			return nil
		}},
		{"AddString", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddString(key, probeSecret)
			return nil
		}},
		{"AddTime", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddTime(key, time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC))
			return nil
		}},
		{"AddUint", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUint(key, 1)
			return nil
		}},
		{"AddUint64", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUint64(key, 2)
			return nil
		}},
		{"AddUint32", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUint32(key, 3)
			return nil
		}},
		{"AddUint16", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUint16(key, 4)
			return nil
		}},
		{"AddUint8", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUint8(key, 5)
			return nil
		}},
		{"AddUintptr", func(enc zapcore.ObjectEncoder, key string) error {
			enc.AddUintptr(key, 6)
			return nil
		}},
	}
}

func TestMaskReplacesBlockedAndForwardsCleanTypedValues(t *testing.T) {
	for _, tc := range probeObjectValues() {
		t.Run(tc.name, func(t *testing.T) {
			reference, referenceLogs := plainLogger()
			plain := probedObject(t, reference, referenceLogs, objectProbe{write: tc.write})

			filtered, filteredLogs := maskedLogger(nil)
			masked := probedObject(t, filtered, filteredLogs, objectProbe{write: tc.write})

			require.Contains(t, masked, "subject")
			require.Contains(t, plain, "password")
			// The reference run writes the decoded Go value -- a uint16, a
			// time, a base64 string -- so the check is that it is not the
			// placeholder, which is what makes the masked run below meaningful.
			assert.NotEqual(t, maskPlaceholder, plain["password"], "the reference run must write the real value")
			assert.Equal(t, maskPlaceholder, masked["password"], "%s must be replaced on a blocked key", tc.name)
			assert.Equal(t, plain["subject"], masked["subject"], "%s must forward a clean field untouched", tc.name)
		})
	}
}

// TestMaskFiltersInsideForwardedNamespace pins that OpenNamespace hands the
// namespace to the inner encoder while keys written beneath it still go
// through this encoder's decision.
func TestMaskFiltersInsideForwardedNamespace(t *testing.T) {
	reference, referenceLogs := plainLogger()
	plain := probedObject(t, reference, referenceLogs, namespaceProbe{})

	filtered, logs := maskedLogger(nil)
	masked := probedObject(t, filtered, logs, namespaceProbe{})

	outerPlain, ok := plain["outer"].(map[string]any)
	require.True(t, ok, "decoded reference namespace actual %T", plain["outer"])
	outer, ok := masked["outer"].(map[string]any)
	require.True(t, ok, "decoded namespace actual %T", masked["outer"])

	assert.NotEqual(t, maskPlaceholder, outerPlain["password"], "the reference run must write the real value")
	assert.Equal(t, maskPlaceholder, outer["password"], "a blocked key inside a forwarded namespace is still replaced")
	assert.Equal(t, outerPlain["user"], outer["user"])
}

// namespaceProbe writes through OpenNamespace itself, which is the only way a
// call reaches that method: a top-level zap.Namespace field goes to the inner
// encoder, so the forwarding branch needs a marshaler to be exercised.
type namespaceProbe struct{}

func (namespaceProbe) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.OpenNamespace("outer")
	enc.AddString("password", probeSecret)
	enc.AddString("user", "alice")
	return nil
}

// TestMaskFiltersReflectedValuesNestedInsideTypedPaths covers the branch that
// only a clean key can reach: the key itself does not hit, but the value
// behind it carries a blocked name, so the reflected result must replace the
// original before it is written.
func TestMaskFiltersReflectedValuesNestedInsideTypedPaths(t *testing.T) {
	nested := func() map[string]any {
		return map[string]any{"password": probeSecret, "user": "alice"}
	}

	t.Run("object field", func(t *testing.T) {
		filtered, logs := maskedLogger(nil)
		probe := probedObject(t, filtered, logs, objectProbe{write: func(enc zapcore.ObjectEncoder, key string) error {
			return enc.AddReflected(key, nested())
		}})

		subject, ok := probe["subject"].(map[string]any)
		require.True(t, ok, "decoded reflected value actual %T", probe["subject"])
		assert.Equal(t, maskPlaceholder, subject["password"])
		assert.Equal(t, "alice", subject["user"])
	})

	t.Run("array element", func(t *testing.T) {
		filtered, logs := maskedLogger(nil)
		element := probedArray(t, filtered, logs, arrayProbe{write: func(enc zapcore.ArrayEncoder) error {
			return enc.AppendReflected(nested())
		}})

		require.Len(t, element, 1)
		subject, ok := element[0].(map[string]any)
		require.True(t, ok, "decoded reflected element actual %T", element[0])
		assert.Equal(t, maskPlaceholder, subject["password"])
		assert.Equal(t, "alice", subject["user"])
	})
}

// arrayWriter writes elements with a single typed ArrayEncoder method.
type arrayWriter func(enc zapcore.ArrayEncoder) error

type arrayProbe struct{ write arrayWriter }

func (p arrayProbe) MarshalLogArray(enc zapcore.ArrayEncoder) error { return p.write(enc) }

func probedArray(t *testing.T, logger *zap.Logger, logs *observer.ObservedLogs, value zapcore.ArrayMarshaler) []any {
	t.Helper()
	logger.Info("probe", zap.Array("probe", value))
	require.Len(t, logs.All(), 1)
	probe, ok := logs.All()[0].ContextMap()["probe"].([]any)
	require.True(t, ok, "decoded probe actual %T", logs.All()[0].ContextMap()["probe"])
	return probe
}

// probeArrayValues executes every scalar Append* forwarder. Array elements
// carry no key, so the property under test is the one these methods are: the
// element reaches the inner encoder exactly as written.
func probeArrayValues() []struct {
	name  string
	write arrayWriter
} {
	return []struct {
		name  string
		write arrayWriter
	}{
		{"AppendArray", func(enc zapcore.ArrayEncoder) error {
			return enc.AppendArray(stringArray{values: []string{probeSecret}})
		}},
		{"AppendObject", func(enc zapcore.ArrayEncoder) error {
			return enc.AppendObject(stringObject{value: probeSecret})
		}},
		{"AppendReflected", func(enc zapcore.ArrayEncoder) error {
			return enc.AppendReflected(map[string]any{"inner": probeSecret})
		}},
		{"AppendBool", func(enc zapcore.ArrayEncoder) error {
			enc.AppendBool(true)
			return nil
		}},
		{"AppendByteString", func(enc zapcore.ArrayEncoder) error {
			enc.AppendByteString([]byte(probeSecret))
			return nil
		}},
		{"AppendComplex128", func(enc zapcore.ArrayEncoder) error {
			enc.AppendComplex128(complex(1, -2))
			return nil
		}},
		{"AppendComplex64", func(enc zapcore.ArrayEncoder) error {
			enc.AppendComplex64(complex64(complex(1, -2)))
			return nil
		}},
		{"AppendDuration", func(enc zapcore.ArrayEncoder) error {
			enc.AppendDuration(1500 * time.Millisecond)
			return nil
		}},
		{"AppendFloat32", func(enc zapcore.ArrayEncoder) error {
			enc.AppendFloat32(1.5)
			return nil
		}},
		{"AppendFloat64", func(enc zapcore.ArrayEncoder) error {
			enc.AppendFloat64(-2.25)
			return nil
		}},
		{"AppendInt", func(enc zapcore.ArrayEncoder) error {
			enc.AppendInt(-1)
			return nil
		}},
		{"AppendInt64", func(enc zapcore.ArrayEncoder) error {
			enc.AppendInt64(-2)
			return nil
		}},
		{"AppendInt32", func(enc zapcore.ArrayEncoder) error {
			enc.AppendInt32(-3)
			return nil
		}},
		{"AppendInt16", func(enc zapcore.ArrayEncoder) error {
			enc.AppendInt16(-4)
			return nil
		}},
		{"AppendInt8", func(enc zapcore.ArrayEncoder) error {
			enc.AppendInt8(-5)
			return nil
		}},
		{"AppendString", func(enc zapcore.ArrayEncoder) error {
			enc.AppendString(probeSecret)
			return nil
		}},
		{"AppendTime", func(enc zapcore.ArrayEncoder) error {
			enc.AppendTime(time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC))
			return nil
		}},
		{"AppendUint", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUint(1)
			return nil
		}},
		{"AppendUint64", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUint64(2)
			return nil
		}},
		{"AppendUint32", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUint32(3)
			return nil
		}},
		{"AppendUint16", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUint16(4)
			return nil
		}},
		{"AppendUint8", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUint8(5)
			return nil
		}},
		{"AppendUintptr", func(enc zapcore.ArrayEncoder) error {
			enc.AppendUintptr(6)
			return nil
		}},
	}
}

func TestMaskArrayPathsForwardElementsUnchanged(t *testing.T) {
	for _, tc := range probeArrayValues() {
		t.Run(tc.name, func(t *testing.T) {
			reference, referenceLogs := plainLogger()
			plain := probedArray(t, reference, referenceLogs, arrayProbe{write: tc.write})

			filtered, filteredLogs := maskedLogger(nil)
			masked := probedArray(t, filtered, filteredLogs, arrayProbe{write: tc.write})

			require.NotEmpty(t, plain, "the reference run must write an element")
			assert.Equal(t, plain, masked, "%s must forward elements untouched", tc.name)
		})
	}
}

// nestArr nests arrays, which is how the array side of the depth cap is
// reached; an object nesting alone never exercises AppendArray's own branch.
type nestArr struct{ left int }

func (n nestArr) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	if n.left == 0 {
		enc.AppendString(probeSecret)
		return nil
	}
	return enc.AppendArray(nestArr{left: n.left - 1})
}

func TestMaskTruncatesOverDeepArray(t *testing.T) {
	const levels = 30
	filtered, logs := maskedLogger(nil)
	require.NotPanics(t, func() { filtered.Info("deep", zap.Array("root", nestArr{left: levels})) })

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	walked := 0
	for walked <= levels {
		sub, ok := cur.([]any)
		if !ok {
			break
		}
		require.Len(t, sub, 1)
		cur = sub[0]
		walked++
	}
	// The fixture nests deeper than the cap, so the walk stops at the cap
	// itself: exactly maxMaskDepth levels survive and the array arriving at the
	// cap is replaced as a whole. Pinning the exact number keeps a regression
	// that counts depth more eagerly -- truncating earlier than documented --
	// from passing as merely "truncated".
	assert.Equal(t, maxMaskDepth, walked, "Exactly the capped number of levels survives")
	assert.Equal(t, maskPlaceholder, cur, "A deep array subtree is replaced as a whole")
}

// nestPair alternates array and object nesting, which is the only shape that
// brings AppendObject to the depth cap: an array of arrays never calls it.
type nestPair struct{ left int }

func (n nestPair) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	if n.left == 0 {
		enc.AppendString(probeSecret)
		return nil
	}
	return enc.AppendObject(nestPair{left: n.left - 1})
}

func (n nestPair) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if n.left == 0 {
		enc.AddString("bottom", probeSecret)
		return nil
	}
	return enc.AddArray("n", nestPair{left: n.left - 1})
}

// TestMaskTruncatesOverDeepAlternatingNesting covers both containers on the
// way down, so each one's own depth branch is taken rather than only the
// branch of whichever container the fixture happens to use.
func TestMaskTruncatesOverDeepAlternatingNesting(t *testing.T) {
	const levels = 30
	filtered, logs := maskedLogger(nil)
	require.NotPanics(t, func() { filtered.Info("deep", zap.Array("root", nestPair{left: levels})) })

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	walked := 0
	for walked <= levels {
		var next any
		switch node := cur.(type) {
		case []any:
			require.Len(t, node, 1)
			next = node[0]
		case map[string]any:
			require.Contains(t, node, "n")
			next = node["n"]
		}
		if next == nil {
			break
		}
		cur = next
		walked++
	}
	assert.Equal(t, maxMaskDepth, walked, "Exactly the capped number of levels survives")
	assert.Equal(t, maskPlaceholder, cur, "A deep alternating subtree is replaced as a whole")
}

// nestArrToObject descends through arrays alone and ends in an object, which
// is the only sequence that brings AppendObject exactly to the cap: in an
// alternating fixture the array side truncates one level earlier, so this
// branch would stay unexecuted.
type nestArrToObject struct{ left int }

func (n nestArrToObject) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	if n.left == 0 {
		return enc.AppendObject(stringObject{value: probeSecret})
	}
	return enc.AppendArray(nestArrToObject{left: n.left - 1})
}

func TestMaskTruncatesOverDeepArrayToObject(t *testing.T) {
	filtered, logs := maskedLogger(nil)
	require.NotPanics(t, func() {
		filtered.Info("deep", zap.Array("root", nestArrToObject{left: maxMaskDepth - 1}))
	})

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	walked := 0
	for {
		node, ok := cur.([]any)
		if !ok {
			break
		}
		require.Len(t, node, 1)
		cur = node[0]
		walked++
		require.LessOrEqual(t, walked, maxMaskDepth, "The cap truncates before the nesting can outgrow it")
	}
	assert.Equal(t, maxMaskDepth, walked, "Exactly the capped number of levels survives")
	// The object is replaced where it was appended, one array deeper than the
	// last array the fixture created.
	assert.Equal(t, maskPlaceholder, cur, "An object arriving at the depth cap is replaced as a whole")
}

// stringObject and stringArray are the smallest marshalers that exercise the
// nesting paths with one observable value.
type stringObject struct{ value string }

func (o stringObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("value", o.value)
	return nil
}

type stringArray struct{ values []string }

func (a stringArray) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	for _, value := range a.values {
		enc.AppendString(value)
	}
	return nil
}
