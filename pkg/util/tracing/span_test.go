// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tracing

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/types"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/trace"
	"google.golang.org/grpc/metadata"
)

func TestRecordingString(t *testing.T) {
	tr := NewTracer()
	tr2 := NewTracer()

	root := tr.StartSpan("root", WithForceRealSpan())
	root.SetVerbose(true)
	root.Record("root 1")
	// Hackily fix the timing on the first log message, so that we can check it later.
	root.i.crdb.mu.recording.recordedLogs[0].Timestamp = root.i.crdb.startTime.Add(time.Millisecond)
	// Sleep a bit so that everything that comes afterwards has higher timestamps
	// than the one we just assigned. Otherwise the sorting will be screwed up.
	time.Sleep(10 * time.Millisecond)

	carrier := metadataCarrier{MD: metadata.MD{}}
	require.NoError(t, tr.InjectMetaInto(root.Meta(), carrier))

	wireSpanMeta, err := tr2.ExtractMetaFrom(carrier)
	require.NoError(t, err)

	remoteChild := tr2.StartSpan("remote child", WithParentAndManualCollection(wireSpanMeta))
	root.Record("root 2")
	remoteChild.Record("remote child 1")
	remoteChild.Finish()

	remoteRec := remoteChild.GetRecording()
	require.NoError(t, root.ImportRemoteSpans(remoteRec))

	root.Record("root 3")

	ch2 := tr.StartSpan("local child", WithParentAndAutoCollection(root))
	root.Record("root 4")
	ch2.Record("local child 1")
	ch2.Finish()

	root.Record("root 5")
	root.Finish()

	rec := root.GetRecording()
	// Sanity check that the recording looks like we want. Note that this is not
	// its String() representation; this just lists all the spans in order.
	require.NoError(t, TestingCheckRecordedSpans(rec, `
		span: root
			tags: _verbose=1
			event: root 1
			event: root 2
			event: root 3
			event: root 4
			event: root 5
			span: remote child
				tags: _verbose=1
				event: remote child 1
			span: local child
				tags: _verbose=1
				event: local child 1
		`))

	require.NoError(t, TestingCheckRecording(rec, `
		=== operation:root _verbose:1
		event:root 1
			=== operation:remote child _verbose:1
			event:remote child 1
		event:root 2
		event:root 3
			=== operation:local child _verbose:1
			event:local child 1
		event:root 4
		event:root 5
		`))
	// Check the timing info on the first two lines.
	lines := strings.Split(rec.String(), "\n")
	l, err := parseLine(lines[0])
	require.NoError(t, err)
	require.Equal(t, traceLine{
		timeSinceTraceStart: "0.000ms",
		timeSincePrev:       "0.000ms",
		text:                "=== operation:root _verbose:1",
	}, l)
	l, err = parseLine(lines[1])
	require.Equal(t, traceLine{
		timeSinceTraceStart: "1.000ms",
		timeSincePrev:       "1.000ms",
		text:                "event:root 1",
	}, l)
	require.NoError(t, err)
}

type traceLine struct {
	timeSinceTraceStart, timeSincePrev string
	text                               string
}

func parseLine(s string) (traceLine, error) {
	// Parse lines like:
	//      0.007ms      0.007ms    event:root 1
	re := regexp.MustCompile(`\s*(.*s)\s*(.*s)\s{4}(.*)`)
	match := re.FindStringSubmatch(s)
	if match == nil {
		return traceLine{}, errors.Newf("line doesn't match: %s", s)
	}
	return traceLine{
		timeSinceTraceStart: match[1],
		timeSincePrev:       match[2],
		text:                match[3],
	}, nil
}

func TestRecordingInRecording(t *testing.T) {
	tr := NewTracer()

	root := tr.StartSpan("root", WithForceRealSpan())
	root.SetVerbose(true)
	child := tr.StartSpan("child", WithParentAndAutoCollection(root), WithForceRealSpan())
	child.SetVerbose(true)
	// The remote grandchild is also recording, however since it's remote the spans
	// have to be imported into the parent manually (this would usually happen via
	// code at the RPC boundaries).
	grandChild := tr.StartSpan("grandchild", WithParentAndManualCollection(child.Meta()))
	grandChild.Finish()
	require.NoError(t, child.ImportRemoteSpans(grandChild.GetRecording()))
	child.Finish()
	root.Finish()

	rootRec := root.GetRecording()
	require.NoError(t, TestingCheckRecordedSpans(rootRec, `
		span: root
			tags: _verbose=1
			span: child
				tags: _verbose=1
				span: grandchild
					tags: _verbose=1
		`))

	childRec := child.GetRecording()
	require.NoError(t, TestingCheckRecordedSpans(childRec, `
		span: child
			tags: _verbose=1
			span: grandchild
				tags: _verbose=1
		`))

	require.NoError(t, TestingCheckRecording(childRec, `
		=== operation:child _verbose:1
			=== operation:grandchild _verbose:1
		`))
}

func TestSpan_ImportRemoteSpans(t *testing.T) {
	// Verify that GetRecording propagates the recording even when the
	// receiving Span isn't verbose.
	tr := NewTracer()
	sp := tr.StartSpan("root", WithForceRealSpan())
	ch := tr.StartSpan("child", WithParentAndManualCollection(sp.Meta()))
	ch.SetVerbose(true)
	ch.Record("foo")
	ch.SetVerbose(false)
	ch.Finish()
	require.NoError(t, sp.ImportRemoteSpans(ch.GetRecording()))
	sp.Finish()

	require.NoError(t, TestingCheckRecordedSpans(sp.GetRecording(), `
		span: root
			span: child
				event: foo
		`))
}

func TestSpanRecordStructured(t *testing.T) {
	tr := NewTracer()
	sp := tr.StartSpan("root", WithForceRealSpan())
	defer sp.Finish()

	sp.RecordStructured(&types.Int32Value{Value: 4})
	rec := sp.GetRecording()
	require.Len(t, rec, 1)
	require.Len(t, rec[0].InternalStructured, 1)
	item := rec[0].InternalStructured[0]
	var d1 types.DynamicAny
	require.NoError(t, types.UnmarshalAny(item, &d1))
	require.IsType(t, (*types.Int32Value)(nil), d1.Message)
	require.NoError(t, TestingCheckRecordedSpans(rec, `
		span: root
			tags: _unfinished=1
		`))
	require.NoError(t, TestingCheckRecording(rec, `
		=== operation:root _unfinished:1
	`))
}

func TestNonVerboseChildSpanRegisteredWithParent(t *testing.T) {
	tr := NewTracer()
	tr._mode = int32(modeBackground)
	sp := tr.StartSpan("root", WithForceRealSpan())
	defer sp.Finish()
	ch := tr.StartSpan("child", WithParentAndAutoCollection(sp), WithForceRealSpan())
	defer ch.Finish()
	require.Len(t, sp.i.crdb.mu.recording.children, 1)
	require.Equal(t, ch.i.crdb, sp.i.crdb.mu.recording.children[0])
	ch.RecordStructured(&types.Int32Value{Value: 5})
	// Check that the child span (incl its payload) is in the recording.
	rec := sp.GetRecording()
	require.Len(t, rec, 2)
	require.Len(t, rec[1].InternalStructured, 1)
}

// TestSpanMaxChildren verifies that a Span can
// track at most maxChildrenPerSpan direct children.
func TestSpanMaxChildren(t *testing.T) {
	tr := NewTracer()
	sp := tr.StartSpan("root", WithForceRealSpan())
	defer sp.Finish()
	for i := 0; i < maxChildrenPerSpan+123; i++ {
		ch := tr.StartSpan(fmt.Sprintf("child %d", i), WithParentAndAutoCollection(sp), WithForceRealSpan())
		ch.Finish()
		exp := i + 1
		if exp > maxChildrenPerSpan {
			exp = maxChildrenPerSpan
		}
		require.Len(t, sp.i.crdb.mu.recording.children, exp)
	}
}

type explodyNetTr struct {
	trace.Trace
}

func (tr *explodyNetTr) Finish() {
	if tr.Trace == nil {
		panic("(*trace.Trace).Finish called twice")
	}
	tr.Trace.Finish()
	tr.Trace = nil
}

// TestSpan_UseAfterFinish finishes a Span multiple times and
// calls all of its methods multiple times as well. This is
// to check that `Span.done` is called in the right places,
// and serves as a regression test for issues such as:
//
// https://github.com/cockroachdb/cockroach/issues/58489#issuecomment-781263005
func TestSpan_UseAfterFinish(t *testing.T) {
	tr := NewTracer()
	tr._useNetTrace = 1
	sp := tr.StartSpan("foo", WithForceRealSpan())
	require.NotNil(t, sp.i.netTr)
	// Set up netTr to reliably explode if Finish'ed twice. We
	// expect `sp.Finish` to not let it come to that.
	sp.i.netTr = &explodyNetTr{Trace: sp.i.netTr}
	sp.Finish()
	require.True(t, sp.done())
	sp.Finish()
	require.EqualValues(t, 2, sp.numFinishCalled)

	netTrT := reflect.TypeOf(sp)
	for i := 0; i < netTrT.NumMethod(); i++ {
		f := netTrT.Method(i)
		t.Run(f.Name, func(t *testing.T) {
			// The receiver is the first argument.
			args := []reflect.Value{reflect.ValueOf(sp)}
			for i := 1; i < f.Type.NumIn(); i++ {
				// Zeroes for the rest. It would be nice to do something
				// like `quick.Check` here (or even just call quick.Check!)
				// but that's for another day. It should be doable!
				args = append(args, reflect.Zero(f.Type.In(i)))
			}
			// NB: on an impl of Span that calls through to `trace.Trace.Finish`, and
			// on my machine, and at the time of writing, `tr.Finish` would reliably
			// deadlock on exactly the 10th call. This motivates the choice of 20
			// below.
			for i := 0; i < 20; i++ {
				t.Run("invoke", func(t *testing.T) {
					if i == 9 {
						f.Func.Call(args)
					} else {
						f.Func.Call(args)
					}
				})
			}
		})
	}
}
