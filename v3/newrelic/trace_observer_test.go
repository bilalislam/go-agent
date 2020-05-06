// +build go1.9
// This build tag is necessary because Infinite Tracing is only supported for Go version 1.9 and up

package newrelic

import (
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/newrelic/go-agent/v3/internal"
	v1 "github.com/newrelic/go-agent/v3/internal/com_newrelic_trace_v1"
)

func TestValidateTraceObserverURL(t *testing.T) {
	testcases := []struct {
		inputHost string
		inputPort int
		expectErr bool
		expectURL *observerURL
	}{
		{
			inputHost: "",
			expectErr: false,
			expectURL: nil,
		},
		{
			inputHost: "testing.com",
			expectErr: false,
			expectURL: &observerURL{
				host:   "testing.com:443",
				secure: true,
			},
		},
		{
			inputHost: "1.2.3.4",
			expectErr: false,
			expectURL: &observerURL{
				host:   "1.2.3.4:443",
				secure: true,
			},
		},
		{
			inputHost: "testing.com",
			inputPort: 123,
			expectErr: false,
			expectURL: &observerURL{
				host:   "testing.com:123",
				secure: true,
			},
		},
		{
			inputHost: "localhost",
			inputPort: 8080,
			expectErr: false,
			expectURL: &observerURL{
				host:   "localhost:8080",
				secure: false,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.inputHost, func(t *testing.T) {
			c := defaultConfig()
			c.DistributedTracer.Enabled = true
			c.SpanEvents.Enabled = true
			c.InfiniteTracing.TraceObserver.Host = tc.inputHost
			if tc.inputPort != 0 {
				c.InfiniteTracing.TraceObserver.Port = tc.inputPort
			}
			url, err := c.validateTraceObserverConfig()

			if tc.expectErr && err == nil {
				t.Error("expected error, received nil")
			} else if !tc.expectErr && err != nil {
				t.Errorf("expected no error, but got one: %s", err)
			}

			if !reflect.DeepEqual(url, tc.expectURL) {
				t.Errorf("url is not as expected: actual=%#v expect=%#v", url, tc.expectURL)
			}
		})
	}
}

func Test8TConfig(t *testing.T) {
	testcases := []struct {
		host         string
		spansEnabled bool
		DTEnabled    bool
		validConfig  bool
	}{
		{
			host:         "localhost",
			spansEnabled: true,
			DTEnabled:    true,
			validConfig:  true,
		},
		{
			host:         "localhost",
			spansEnabled: false,
			DTEnabled:    true,
			validConfig:  false,
		},
		{
			host:         "localhost",
			spansEnabled: true,
			DTEnabled:    false,
			validConfig:  false,
		},
		{
			host:         "localhost",
			spansEnabled: false,
			DTEnabled:    false,
			validConfig:  false,
		},
		{
			host:         "",
			spansEnabled: false,
			DTEnabled:    false,
			validConfig:  true,
		},
	}

	for _, test := range testcases {
		cfg := Config{}
		cfg.License = testLicenseKey
		cfg.AppName = "app"
		cfg.InfiniteTracing.TraceObserver.Host = test.host
		cfg.SpanEvents.Enabled = test.spansEnabled
		cfg.DistributedTracer.Enabled = test.DTEnabled

		_, err := newInternalConfig(cfg, func(s string) string { return "" }, []string{})
		if (err == nil) != test.validConfig {
			t.Errorf("Infite Tracing config validation failed: %v", test)
		}
	}
}

func TestTraceObserverRoundTrip(t *testing.T) {
	s := newTestObsServer(t)
	defer s.Close()
	runToken := "aRunToken"
	app := testAppBlockOnTrObs(DTReplyFieldsWithTrObsDialer(s.dialer, runToken), toCfgWithTrObserver, t)
	txn := app.StartTransaction("txn1")
	txn.StartSegment("seg1").End()
	txn.End()
	app.Shutdown(10 * time.Second)
	app.expectNoLoggedErrors(t)

	// Ensure no spans were sent the normal way
	app.ExpectSpanEvents(t, nil)

	s.WaitForSpans(t, 2, time.Second)
	s.ExpectMetadata(t, map[string]string{
		"agent_run_token": runToken,
		"license_key":     testLicenseKey,
	})
}

type expectServer struct {
	metadata metadata.MD
	sync.Mutex

	spansReceivedChan chan struct{}
}

func (s *expectServer) RecordSpan(stream v1.IngestService_RecordSpanServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if ok {
		s.Lock()
		s.metadata = md
		s.Unlock()
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		} else if nil != err {
			return err
		}
		s.spansReceivedChan <- struct{}{}
	}
}

func (s *expectServer) ExpectMetadata(t internal.Validator, want map[string]string) {
	s.Lock()
	actualMetadataLen := len(s.metadata)
	s.Unlock()

	extraMetadata := map[string]string{
		":authority":   internal.MatchAnyString,
		"content-type": internal.MatchAnyString,
		"user-agent":   internal.MatchAnyString,
	}

	want = mergeMetadata(want, extraMetadata)

	if len(want) != actualMetadataLen {
		t.Error("length of metadata is incorrect: expected/actual", len(want), actualMetadataLen)
		return
	}

	s.Lock()
	actual := s.metadata
	s.Unlock()
	for key, expectedVal := range want {
		found, ok := actual[key]
		actualVal := strings.Join(found, ",")
		if !ok {
			t.Error("expected metadata not found: ", key)
			continue
		}
		if expectedVal == internal.MatchAnyString {
			continue
		}
		if actualVal != expectedVal {
			t.Error("metadata value difference - expected/actual",
				fmt.Sprintf("key=%s", key), expectedVal, actualVal)
		}
	}
	for key, val := range actual {
		_, ok := want[key]
		if !ok {
			t.Error("unexpected metadata present", key, val)
			continue
		}
	}
}

// Add the `extraMetadata` to each of the maps in the `want` parameter.
// The data in `want` takes precedence over the `extraMetadata`.
func mergeMetadata(want map[string]string, extraMetadata map[string]string) map[string]string {
	newMap := make(map[string]string)
	for k, v := range extraMetadata {
		newMap[k] = v
	}
	for k, v := range want {
		newMap[k] = v
	}
	return newMap
}

// testObsServer contains an in-memory grpc.Server and associated information
// needed to connect to it and verify the data it receives
type testObsServer struct {
	*expectServer
	server *grpc.Server
	conn   *grpc.ClientConn
	dialer internal.DialerFunc
}

func (ts *testObsServer) Close() {
	ts.conn.Close()
	ts.server.Stop()
}

// newTestObsServer creates a new testObsServer for use in testing. Be sure
// to Close() the server when done with it.
func newTestObsServer(t *testing.T) testObsServer {
	grpcServer := grpc.NewServer()
	s := &expectServer{
		// Hard coding the buffer to 10 for now, but it could be variable if needed later.
		spansReceivedChan: make(chan struct{}, 10),
	}
	v1.RegisterIngestServiceServer(grpcServer, s)
	lis := bufconn.Listen(1024 * 1024)

	go grpcServer.Serve(lis)

	bufDialer := func(string, time.Duration) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.Dial("bufnet",
		grpc.WithDialer(bufDialer),
		grpc.WithInsecure(),
		grpc.WithBlock(), // create the connection synchronously
	)
	if err != nil {
		t.Fatal("failure to create ClientConn", err)
	}
	return testObsServer{
		expectServer: s,
		server:       grpcServer,
		conn:         conn,
		dialer:       bufDialer,
	}
}

func (s *expectServer) WaitForSpans(t *testing.T, expected int, secTimeout time.Duration) {
	var rcvd int
	timeout := time.NewTicker(secTimeout)
	defer timeout.Stop()
	for {
		select {
		case <-s.spansReceivedChan:
			rcvd++
			if rcvd >= expected {
				return
			}
		case <-timeout.C:
			t.Errorf("Did not receive expected spans before timeout - expected %d but got %d", expected, rcvd)
			return
		}
	}
}

// testAppBlockOnTrObs is to be used when creating a test application that needs to block
// until the trace observer (which should be configured in the cfgfn) has connected.
func testAppBlockOnTrObs(replyfn func(*internal.ConnectReply), cfgfn func(*Config), t testing.TB) *expectApp {
	app := testApp(replyfn, cfgfn, t)
	app.app.connectTraceObserver(app.app.placeholderRun.Reply)
	timeout := time.NewTicker(3 * time.Second)
	defer timeout.Stop()
	select {
	case <-app.app.TraceObserver.initialConnSuccess:
		return &app
	case <-timeout.C:
		t.Fatal("Error connecting to trace observer")
		return nil
	}
}

func DTReplyFieldsWithTrObsDialer(d internal.DialerFunc, runToken string) func(*internal.ConnectReply) {
	return func(reply *internal.ConnectReply) {
		distributedTracingReplyFields(reply)
		reply.RunID = internal.AgentRunID(runToken)
		reply.TraceObsDialer = d
	}
}

func toCfgWithTrObserver(cfg *Config) {
	cfg.CrossApplicationTracer.Enabled = false
	cfg.DistributedTracer.Enabled = true
	cfg.InfiniteTracing.TraceObserver.Host = "localhost"
}

func TestTraceObserverErrToCodeString(t *testing.T) {
	// if the grpc code names change upstream, this test will alert us to that
	testcases := []struct {
		code   codes.Code
		expect string
	}{
		{code: 0, expect: "OK"},
		{code: 1, expect: "CANCELED"},
		{code: 2, expect: "UNKNOWN"},
		{code: 3, expect: "INVALIDARGUMENT"},
		{code: 4, expect: "DEADLINEEXCEEDED"},
		{code: 5, expect: "NOTFOUND"},
		{code: 6, expect: "ALREADYEXISTS"},
		{code: 7, expect: "PERMISSIONDENIED"},
		{code: 8, expect: "RESOURCEEXHAUSTED"},
		{code: 9, expect: "FAILEDPRECONDITION"},
		{code: 10, expect: "ABORTED"},
		{code: 11, expect: "OUTOFRANGE"},
		{code: 12, expect: "UNIMPLEMENTED"},
		{code: 13, expect: "INTERNAL"},
		{code: 14, expect: "UNAVAILABLE"},
		{code: 15, expect: "DATALOSS"},
		{code: 16, expect: "UNAUTHENTICATED"},
		// we should always test one more than the number of codes supported by
		// grpc so we can detect when a new code is added
		{code: 17, expect: "CODE(17)"},
	}
	for _, test := range testcases {
		t.Run(test.expect, func(t *testing.T) {
			err := status.Error(test.code, "oops")
			actual := errToCodeString(err)
			if actual != test.expect {
				t.Errorf("incorrect error string returned: actual=%s expected=%s",
					actual, test.expect)
			}
		})
	}
}
