/*
Copyright 2015 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bigtable // import "cloud.google.com/go/bigtable"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	btopt "cloud.google.com/go/bigtable/internal/option"
	"cloud.google.com/go/internal/trace"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
	"google.golang.org/api/option/internaloption"
	gtransport "google.golang.org/api/transport/grpc"
	btpb "google.golang.org/genproto/googleapis/bigtable/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	// Install google-c2p resolver, which is required for direct path.
	_ "google.golang.org/grpc/xds/googledirectpath"
	// Install RLS load balancer policy, which is needed for gRPC RLS.
	_ "google.golang.org/grpc/balancer/rls"
)

const prodAddr = "bigtable.googleapis.com:443"
const mtlsProdAddr = "bigtable.mtls.googleapis.com:443"

// Client is a client for reading and writing data to tables in an instance.
//
// A Client is safe to use concurrently, except for its Close method.
type Client struct {
	connPool          gtransport.ConnPool
	client            btpb.BigtableClient
	project, instance string
	appProfile        string
}

// ClientConfig has configurations for the client.
type ClientConfig struct {
	// The id of the app profile to associate with all data operations sent from this client.
	// If unspecified, the default app profile for the instance will be used.
	AppProfile string
}

// NewClient creates a new Client for a given project and instance.
// The default ClientConfig will be used.
func NewClient(ctx context.Context, project, instance string, opts ...option.ClientOption) (*Client, error) {
	return NewClientWithConfig(ctx, project, instance, ClientConfig{}, opts...)
}

// NewClientWithConfig creates a new client with the given config.
func NewClientWithConfig(ctx context.Context, project, instance string, config ClientConfig, opts ...option.ClientOption) (*Client, error) {
	o, err := btopt.DefaultClientOptions(prodAddr, mtlsProdAddr, Scope, clientUserAgent)
	if err != nil {
		return nil, err
	}
	// Add gRPC client interceptors to supply Google client information. No external interceptors are passed.
	o = append(o, btopt.ClientInterceptorOptions(nil, nil)...)

	// Default to a small connection pool that can be overridden.
	o = append(o,
		option.WithGRPCConnectionPool(4),
		// Set the max size to correspond to server-side limits.
		option.WithGRPCDialOption(grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(1<<28), grpc.MaxCallRecvMsgSize(1<<28))),
	)
	// Attempts direct access to spanner service over gRPC to improve throughput,
	// whether the attempt is allowed is totally controlled by service owner.
	o = append(o, internaloption.EnableDirectPath(true))
	o = append(o, opts...)
	connPool, err := gtransport.DialPool(ctx, o...)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	return &Client{
		connPool:   connPool,
		client:     btpb.NewBigtableClient(connPool),
		project:    project,
		instance:   instance,
		appProfile: config.AppProfile,
	}, nil
}

// Close closes the Client.
func (c *Client) Close() error {
	return c.connPool.Close()
}

var (
	idempotentRetryCodes  = []codes.Code{codes.DeadlineExceeded, codes.Unavailable, codes.Aborted}
	isIdempotentRetryCode = make(map[codes.Code]bool)
	retryOptions          = []gax.CallOption{
		gax.WithRetry(func() gax.Retryer {
			return gax.OnCodes(idempotentRetryCodes, gax.Backoff{
				Initial:    100 * time.Millisecond,
				Max:        2 * time.Second,
				Multiplier: 1.2,
			})
		}),
	}
)

func init() {
	for _, code := range idempotentRetryCodes {
		isIdempotentRetryCode[code] = true
	}
}

func (c *Client) fullTableName(table string) string {
	return fmt.Sprintf("projects/%s/instances/%s/tables/%s", c.project, c.instance, table)
}

func (c *Client) requestParamsHeaderValue(table string) string {
	return fmt.Sprintf("table_name=%s&app_profile_id=%s", url.QueryEscape(c.fullTableName(table)), url.QueryEscape(c.appProfile))
}

// mergeOutgoingMetadata returns a context populated by the existing outgoing
// metadata merged with the provided mds.
func mergeOutgoingMetadata(ctx context.Context, mds ...metadata.MD) context.Context {
	ctxMD, _ := metadata.FromOutgoingContext(ctx)
	// The ordering matters, hence why ctxMD comes first.
	allMDs := append([]metadata.MD{ctxMD}, mds...)
	return metadata.NewOutgoingContext(ctx, metadata.Join(allMDs...))
}

// A Table refers to a table.
//
// A Table is safe to use concurrently.
type Table struct {
	c     *Client
	table string

	// Metadata to be sent with each request.
	md metadata.MD
}

// Open opens a table.
func (c *Client) Open(table string) *Table {
	return &Table{
		c:     c,
		table: table,
		md: metadata.Join(metadata.Pairs(
			resourcePrefixHeader, c.fullTableName(table),
			requestParamsHeader, c.requestParamsHeaderValue(table),
		), btopt.WithFeatureFlags()),
	}
}

// TODO(dsymonds): Read method that returns a sequence of ReadItems.

// ReadRows reads rows from a table. f is called for each row.
// If f returns false, the stream is shut down and ReadRows returns.
// f owns its argument, and f is called serially in order by row key.
//
// By default, the yielded rows will contain all values in all cells.
// Use RowFilter to limit the cells returned.
func (t *Table) ReadRows(ctx context.Context, arg RowSet, f func(Row) bool, opts ...ReadOption) (err error) {
	ctx = mergeOutgoingMetadata(ctx, t.md)
	ctx = trace.StartSpan(ctx, "cloud.google.com/go/bigtable.ReadRows")
	defer func() { trace.EndSpan(ctx, err) }()

	var prevRowKey string
	attrMap := make(map[string]interface{})
	err = gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		if !arg.valid() {
			// Empty row set, no need to make an API call.
			// NOTE: we must return early if arg == RowList{} because reading
			// an empty RowList from bigtable returns all rows from that table.
			return nil
		}
		req := &btpb.ReadRowsRequest{
			TableName:    t.c.fullTableName(t.table),
			AppProfileId: t.c.appProfile,
			Rows:         arg.proto(),
		}
		settings := makeReadSettings(req)
		for _, opt := range opts {
			opt.set(&settings)
		}
		ctx, cancel := context.WithCancel(ctx) // for aborting the stream
		defer cancel()

		startTime := time.Now()
		stream, err := t.c.client.ReadRows(ctx, req)
		if err != nil {
			return err
		}

		var cr *chunkReader
		if req.Reversed {
			cr = newReverseChunkReader()
		} else {
			cr = newChunkReader()
		}

		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				// Reset arg for next Invoke call.
				if req.Reversed {
					arg = arg.retainRowsBefore(prevRowKey)
				} else {
					arg = arg.retainRowsAfter(prevRowKey)
				}
				attrMap["rowKey"] = prevRowKey
				attrMap["error"] = err.Error()
				attrMap["time_secs"] = time.Since(startTime).Seconds()
				trace.TracePrintf(ctx, attrMap, "Retry details in ReadRows")
				return err
			}
			attrMap["time_secs"] = time.Since(startTime).Seconds()
			attrMap["rowCount"] = len(res.Chunks)
			trace.TracePrintf(ctx, attrMap, "Details in ReadRows")

			for _, cc := range res.Chunks {
				row, err := cr.Process(cc)
				if err != nil {
					// No need to prepare for a retry, this is an unretryable error.
					return err
				}
				if row == nil {
					continue
				}
				prevRowKey = row.Key()
				if !f(row) {
					// Cancel and drain stream.
					cancel()
					for {
						if _, err := stream.Recv(); err != nil {
							// The stream has ended. We don't return an error
							// because the caller has intentionally interrupted the scan.
							return nil
						}
					}
				}
			}

			if res.LastScannedRowKey != nil {
				prevRowKey = string(res.LastScannedRowKey)
			}

			// Handle any incoming RequestStats. This should happen at most once.
			if res.RequestStats != nil && settings.fullReadStatsFunc != nil {
				stats := makeFullReadStats(res.RequestStats)
				settings.fullReadStatsFunc(&stats)
			}

			if err := cr.Close(); err != nil {
				// No need to prepare for a retry, this is an unretryable error.
				return err
			}
		}
		return err
	}, retryOptions...)

	// Convert error to grpc status error
	if err != nil {
		if errStatus, ok := status.FromError(err); ok {
			return status.Error(errStatus.Code(), errStatus.Message())
		}

		ctxStatus := status.FromContextError(err)
		if ctxStatus.Code() != codes.Unknown {
			return status.Error(ctxStatus.Code(), ctxStatus.Message())
		}
	}

	return err
}

// ReadRow is a convenience implementation of a single-row reader.
// A missing row will return nil for both Row and error.
func (t *Table) ReadRow(ctx context.Context, row string, opts ...ReadOption) (Row, error) {
	var r Row

	opts = append([]ReadOption{LimitRows(1)}, opts...)
	err := t.ReadRows(ctx, SingleRow(row), func(rr Row) bool {
		r = rr
		return true
	}, opts...)
	return r, err
}

// decodeFamilyProto adds the cell data from f to the given row.
func decodeFamilyProto(r Row, row string, f *btpb.Family) {
	fam := f.Name // does not have colon
	for _, col := range f.Columns {
		for _, cell := range col.Cells {
			ri := ReadItem{
				Row:       row,
				Column:    fam + ":" + string(col.Qualifier),
				Timestamp: Timestamp(cell.TimestampMicros),
				Value:     cell.Value,
			}
			r[fam] = append(r[fam], ri)
		}
	}
}

// RowSet is a set of rows to be read. It is satisfied by RowList, RowRange and RowRangeList.
// The serialized size of the RowSet must be no larger than 1MiB.
type RowSet interface {
	proto() *btpb.RowSet

	// retainRowsAfter returns a new RowSet that does not include the
	// given row key or any row key lexicographically less than it.
	retainRowsAfter(lastRowKey string) RowSet

	// retainRowsBefore returns a new RowSet that does not include the
	// given row key or any row key lexicographically greater than it.
	retainRowsBefore(lastRowKey string) RowSet

	// Valid reports whether this set can cover at least one row.
	valid() bool
}

// RowList is a sequence of row keys.
type RowList []string

func (r RowList) proto() *btpb.RowSet {
	keys := make([][]byte, len(r))
	for i, row := range r {
		keys[i] = []byte(row)
	}
	return &btpb.RowSet{RowKeys: keys}
}

func (r RowList) retainRowsAfter(lastRowKey string) RowSet {
	var retryKeys RowList
	for _, key := range r {
		if key > lastRowKey {
			retryKeys = append(retryKeys, key)
		}
	}
	return retryKeys
}

func (r RowList) retainRowsBefore(lastRowKey string) RowSet {
	var retryKeys RowList
	for _, key := range r {
		if key < lastRowKey {
			retryKeys = append(retryKeys, key)
		}
	}
	return retryKeys
}

func (r RowList) valid() bool {
	return len(r) > 0
}

type rangeBoundType int64

const (
	rangeUnbounded rangeBoundType = iota
	rangeOpen
	rangeClosed
)

// A RowRange describes a range of rows between the start and end key. Start and
// end keys may be rangeOpen, rangeClosed or rangeUnbounded.
type RowRange struct {
	startBound rangeBoundType
	start      string
	endBound   rangeBoundType
	end        string
}

// NewRange returns the new RowRange [begin, end).
func NewRange(begin, end string) RowRange {
	return createRowRange(rangeClosed, begin, rangeOpen, end)
}

// NewClosedOpenRange returns the RowRange consisting of all greater than or
// equal to the start and less than the end: [start, end).
func NewClosedOpenRange(start, end string) RowRange {
	return createRowRange(rangeClosed, start, rangeOpen, end)
}

// NewOpenClosedRange returns the RowRange consisting of all keys greater than
// the start and less than or equal to the end: (start, end].
func NewOpenClosedRange(start, end string) RowRange {
	return createRowRange(rangeOpen, start, rangeClosed, end)
}

// NewOpenRange returns the RowRange consisting of all keys greater than the
// start and less than the end: (start, end).
func NewOpenRange(start, end string) RowRange {
	return createRowRange(rangeOpen, start, rangeOpen, end)
}

// NewClosedRange returns the RowRange consisting of all keys greater than or
// equal to the start and less than or equal to the end: [start, end].
func NewClosedRange(start, end string) RowRange {
	return createRowRange(rangeClosed, start, rangeClosed, end)
}

// PrefixRange returns a RowRange consisting of all keys starting with the prefix.
func PrefixRange(prefix string) RowRange {
	end := prefixSuccessor(prefix)
	return createRowRange(rangeClosed, prefix, rangeOpen, end)
}

// InfiniteRange returns the RowRange consisting of all keys at least as
// large as start: [start, ∞).
func InfiniteRange(start string) RowRange {
	return createRowRange(rangeClosed, start, rangeUnbounded, "")
}

// InfiniteReverseRange returns the RowRange consisting of all keys less than or
// equal to the end: (∞, end].
func InfiniteReverseRange(end string) RowRange {
	return createRowRange(rangeUnbounded, "", rangeClosed, end)
}

// createRowRange creates a new RowRange, normalizing start and end
// rangeBoundType to rangeUnbounded if they're empty strings because empty
// strings also represent unbounded keys
func createRowRange(startBound rangeBoundType, start string, endBound rangeBoundType, end string) RowRange {
	// normalize start bound type
	if start == "" {
		startBound = rangeUnbounded
	}
	// normalize end bound type
	if end == "" {
		endBound = rangeUnbounded
	}
	return RowRange{
		startBound: startBound,
		start:      start,
		endBound:   endBound,
		end:        end,
	}
}

// Unbounded tests whether a RowRange is unbounded.
func (r RowRange) Unbounded() bool {
	return r.startBound == rangeUnbounded || r.endBound == rangeUnbounded
}

// Contains says whether the RowRange contains the key.
func (r RowRange) Contains(row string) bool {
	switch r.startBound {
	case rangeOpen:
		if r.start >= row {
			return false
		}
	case rangeClosed:
		if r.start > row {
			return false
		}
	case rangeUnbounded:
	}

	switch r.endBound {
	case rangeOpen:
		if r.end <= row {
			return false
		}
	case rangeClosed:
		if r.end < row {
			return false
		}
	case rangeUnbounded:
	}

	return true
}

// String provides a printable description of a RowRange.
func (r RowRange) String() string {
	var startStr string
	switch r.startBound {
	case rangeOpen:
		startStr = "(" + strconv.Quote(r.start)
	case rangeClosed:
		startStr = "[" + strconv.Quote(r.start)
	case rangeUnbounded:
		startStr = "(∞"
	}

	var endStr string
	switch r.endBound {
	case rangeOpen:
		endStr = r.end + ")"
	case rangeClosed:
		endStr = r.end + "]"
	case rangeUnbounded:
		endStr = "∞)"
	}

	return fmt.Sprintf("%s,%s", startStr, endStr)
}

func (r RowRange) proto() *btpb.RowSet {
	rr := &btpb.RowRange{}

	switch r.startBound {
	case rangeOpen:
		rr.StartKey = &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte(r.start)}
	case rangeClosed:
		rr.StartKey = &btpb.RowRange_StartKeyClosed{StartKeyClosed: []byte(r.start)}
	case rangeUnbounded:
		// leave unbounded
	}

	switch r.endBound {
	case rangeOpen:
		rr.EndKey = &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte(r.end)}
	case rangeClosed:
		rr.EndKey = &btpb.RowRange_EndKeyClosed{EndKeyClosed: []byte(r.end)}
	case rangeUnbounded:
		// leave unbounded
	}

	return &btpb.RowSet{RowRanges: []*btpb.RowRange{rr}}
}

func (r RowRange) retainRowsAfter(lastRowKey string) RowSet {
	if lastRowKey == "" || lastRowKey < r.start {
		return r
	}

	return RowRange{
		// Set the beginning of the range to the row after the last scanned.
		startBound: rangeOpen,
		start:      lastRowKey,
		endBound:   r.endBound,
		end:        r.end,
	}
}

func (r RowRange) retainRowsBefore(lastRowKey string) RowSet {
	if lastRowKey == "" || (r.endBound != rangeUnbounded && r.end < lastRowKey) {
		return r
	}

	return RowRange{
		startBound: r.startBound,
		start:      r.start,
		endBound:   rangeOpen,
		end:        lastRowKey,
	}
}

func (r RowRange) valid() bool {
	// If either end is unbounded, then the range is always valid.
	if r.Unbounded() {
		return true
	}

	// If either end is an open interval, then the start must be strictly less
	// than the end and since neither end is unbounded, we don't have to check
	// for empty strings.
	if r.startBound == rangeOpen || r.endBound == rangeOpen {
		return r.start < r.end
	}

	// At this point both endpoints must be closed, which makes [a,a] a valid
	// interval
	return r.start <= r.end
}

// RowRangeList is a sequence of RowRanges representing the union of the ranges.
type RowRangeList []RowRange

func (r RowRangeList) proto() *btpb.RowSet {
	ranges := make([]*btpb.RowRange, len(r))
	for i, rr := range r {
		// RowRange.proto() returns a RowSet with a single element RowRange array
		ranges[i] = rr.proto().RowRanges[0]
	}
	return &btpb.RowSet{RowRanges: ranges}
}

func (r RowRangeList) retainRowsAfter(lastRowKey string) RowSet {
	if lastRowKey == "" {
		return r
	}
	// Return a list of any range that has not yet been completely processed
	var ranges RowRangeList
	for _, rr := range r {
		retained := rr.retainRowsAfter(lastRowKey)
		if retained.valid() {
			ranges = append(ranges, retained.(RowRange))
		}
	}
	return ranges
}

func (r RowRangeList) retainRowsBefore(lastRowKey string) RowSet {
	if lastRowKey == "" {
		return r
	}
	// Return a list of any range that has not yet been completely processed
	var ranges RowRangeList
	for _, rr := range r {
		retained := rr.retainRowsBefore(lastRowKey)
		if retained.valid() {
			ranges = append(ranges, retained.(RowRange))
		}
	}
	return ranges
}

func (r RowRangeList) valid() bool {
	for _, rr := range r {
		if rr.valid() {
			return true
		}
	}
	return false
}

// SingleRow returns a RowSet for reading a single row.
func SingleRow(row string) RowSet {
	return RowList{row}
}

// prefixSuccessor returns the lexically smallest string greater than the
// prefix, if it exists, or "" otherwise.  In either case, it is the string
// needed for the Limit of a RowRange.
func prefixSuccessor(prefix string) string {
	if prefix == "" {
		return "" // infinite range
	}
	n := len(prefix)
	for n--; n >= 0 && prefix[n] == '\xff'; n-- {
	}
	if n == -1 {
		return ""
	}
	ans := []byte(prefix[:n])
	ans = append(ans, prefix[n]+1)
	return string(ans)
}

// ReadIterationStats captures information about the iteration of rows or cells over the course of
// a read, e.g. how many results were scanned in a read operation versus the results returned.
type ReadIterationStats struct {
	// The cells returned as part of the request.
	CellsReturnedCount int64

	// The cells seen (scanned) as part of the request. This includes the count of cells returned, as
	// captured below.
	CellsSeenCount int64

	// The rows returned as part of the request.
	RowsReturnedCount int64

	// The rows seen (scanned) as part of the request. This includes the count of rows returned, as
	// captured below.
	RowsSeenCount int64
}

// RequestLatencyStats provides a measurement of the latency of the request as it interacts with
// different systems over its lifetime, e.g. how long the request took to execute within a frontend
// server.
type RequestLatencyStats struct {
	// The latency measured by the frontend server handling this request, from when the request was
	// received, to when this value is sent back in the response. For more context on the component
	// that is measuring this latency, see: https://cloud.google.com/bigtable/docs/overview
	FrontendServerLatency time.Duration
}

// FullReadStats captures all known information about a read.
type FullReadStats struct {
	// Iteration stats describe how efficient the read is, e.g. comparing rows seen vs. rows
	// returned or cells seen vs cells returned can provide an indication of read efficiency
	// (the higher the ratio of seen to retuned the better).
	ReadIterationStats ReadIterationStats

	// Request latency stats describe the time taken to complete a request, from the server
	// side.
	RequestLatencyStats RequestLatencyStats
}

// Returns a FullReadStats populated from a RequestStats. This assumes the stats view is
// REQUEST_STATS_FULL. That is the only stats view currently supported.
func makeFullReadStats(reqStats *btpb.RequestStats) FullReadStats {
	statsView := reqStats.GetFullReadStatsView()
	readStats := statsView.ReadIterationStats
	latencyStats := statsView.RequestLatencyStats
	return FullReadStats{
		ReadIterationStats: ReadIterationStats{
			CellsReturnedCount: readStats.CellsReturnedCount,
			CellsSeenCount:     readStats.CellsSeenCount,
			RowsReturnedCount:  readStats.RowsReturnedCount,
			RowsSeenCount:      readStats.RowsSeenCount},
		RequestLatencyStats: RequestLatencyStats{
			FrontendServerLatency: latencyStats.FrontendServerLatency.AsDuration()}}
}

// FullReadStatsFunc describes a callback that receives a FullReadStats for evaluation.
type FullReadStatsFunc func(*FullReadStats)

// readSettings is a collection of objects that can be modified by ReadOption instances to apply settings.
type readSettings struct {
	req               *btpb.ReadRowsRequest
	fullReadStatsFunc FullReadStatsFunc
}

func makeReadSettings(req *btpb.ReadRowsRequest) readSettings {
	return readSettings{req, nil}
}

// A ReadOption is an optional argument to ReadRows.
type ReadOption interface {
	set(settings *readSettings)
}

// RowFilter returns a ReadOption that applies f to the contents of read rows.
//
// If multiple RowFilters are provided, only the last is used. To combine filters,
// use ChainFilters or InterleaveFilters instead.
func RowFilter(f Filter) ReadOption { return rowFilter{f} }

type rowFilter struct{ f Filter }

func (rf rowFilter) set(settings *readSettings) { settings.req.Filter = rf.f.proto() }

// LimitRows returns a ReadOption that will end the number of rows to be read.
func LimitRows(limit int64) ReadOption { return limitRows{limit} }

type limitRows struct{ limit int64 }

func (lr limitRows) set(settings *readSettings) { settings.req.RowsLimit = lr.limit }

// WithFullReadStats returns a ReadOption that will request FullReadStats
// and invoke the given callback on the resulting FullReadStats.
func WithFullReadStats(f FullReadStatsFunc) ReadOption { return withFullReadStats{f} }

type withFullReadStats struct {
	f FullReadStatsFunc
}

func (wrs withFullReadStats) set(settings *readSettings) {
	settings.req.RequestStatsView = btpb.ReadRowsRequest_REQUEST_STATS_FULL
	settings.fullReadStatsFunc = wrs.f
}

// ReverseScan returns a RadOption that will reverse the results of a Scan.
// The rows will be streamed in reverse lexiographic order of the keys. The row key ranges of the RowSet are
// still expected to be oriented the same way as forwards. ie [a,c] where a <= c. The row content
// will remain unchanged from the ordering forward scans. This is particularly useful to get the
// last N records before a key:
//
//	table.ReadRows(ctx, NewOpenClosedRange("", "key"), func(row bigtable.Row) bool {
//	   return true
//	}, bigtable.ReverseScan(), bigtable.LimitRows(10))
func ReverseScan() ReadOption {
	return reverseScan{}
}

type reverseScan struct{}

func (rs reverseScan) set(settings *readSettings) {
	settings.req.Reversed = true
}

// mutationsAreRetryable returns true if all mutations are idempotent
// and therefore retryable. A mutation is idempotent iff all cell timestamps
// have an explicit timestamp set and do not rely on the timestamp being set on the server.
func mutationsAreRetryable(muts []*btpb.Mutation) bool {
	serverTime := int64(ServerTime)
	for _, mut := range muts {
		setCell := mut.GetSetCell()
		if setCell != nil && setCell.TimestampMicros == serverTime {
			return false
		}
	}
	return true
}

const maxMutations = 100000

// Apply mutates a row atomically. A mutation must contain at least one
// operation and at most 100000 operations.
func (t *Table) Apply(ctx context.Context, row string, m *Mutation, opts ...ApplyOption) (err error) {
	ctx = mergeOutgoingMetadata(ctx, t.md)
	ctx = trace.StartSpan(ctx, "cloud.google.com/go/bigtable/Apply")
	defer func() { trace.EndSpan(ctx, err) }()

	after := func(res proto.Message) {
		for _, o := range opts {
			o.after(res)
		}
	}

	var callOptions []gax.CallOption
	if m.cond == nil {
		req := &btpb.MutateRowRequest{
			TableName:    t.c.fullTableName(t.table),
			AppProfileId: t.c.appProfile,
			RowKey:       []byte(row),
			Mutations:    m.ops,
		}
		if mutationsAreRetryable(m.ops) {
			callOptions = retryOptions
		}
		var res *btpb.MutateRowResponse
		err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
			var err error
			res, err = t.c.client.MutateRow(ctx, req)
			return err
		}, callOptions...)
		if err == nil {
			after(res)
		}
		return err
	}

	req := &btpb.CheckAndMutateRowRequest{
		TableName:       t.c.fullTableName(t.table),
		AppProfileId:    t.c.appProfile,
		RowKey:          []byte(row),
		PredicateFilter: m.cond.proto(),
	}
	if m.mtrue != nil {
		if m.mtrue.cond != nil {
			return errors.New("bigtable: conditional mutations cannot be nested")
		}
		req.TrueMutations = m.mtrue.ops
	}
	if m.mfalse != nil {
		if m.mfalse.cond != nil {
			return errors.New("bigtable: conditional mutations cannot be nested")
		}
		req.FalseMutations = m.mfalse.ops
	}
	if mutationsAreRetryable(req.TrueMutations) && mutationsAreRetryable(req.FalseMutations) {
		callOptions = retryOptions
	}
	var cmRes *btpb.CheckAndMutateRowResponse
	err = gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		var err error
		cmRes, err = t.c.client.CheckAndMutateRow(ctx, req)
		return err
	}, callOptions...)
	if err == nil {
		after(cmRes)
	}
	return err
}

// An ApplyOption is an optional argument to Apply.
type ApplyOption interface {
	after(res proto.Message)
}

type applyAfterFunc func(res proto.Message)

func (a applyAfterFunc) after(res proto.Message) { a(res) }

// GetCondMutationResult returns an ApplyOption that reports whether the conditional
// mutation's condition matched.
func GetCondMutationResult(matched *bool) ApplyOption {
	return applyAfterFunc(func(res proto.Message) {
		if res, ok := res.(*btpb.CheckAndMutateRowResponse); ok {
			*matched = res.PredicateMatched
		}
	})
}

// Mutation represents a set of changes for a single row of a table.
type Mutation struct {
	ops []*btpb.Mutation

	// for conditional mutations
	cond          Filter
	mtrue, mfalse *Mutation
}

// NewMutation returns a new mutation.
func NewMutation() *Mutation {
	return new(Mutation)
}

// NewCondMutation returns a conditional mutation.
// The given row filter determines which mutation is applied:
// If the filter matches any cell in the row, mtrue is applied;
// otherwise, mfalse is applied.
// Either given mutation may be nil.
//
// The application of a ReadModifyWrite is atomic; concurrent ReadModifyWrites will
// be executed serially by the server.
func NewCondMutation(cond Filter, mtrue, mfalse *Mutation) *Mutation {
	return &Mutation{cond: cond, mtrue: mtrue, mfalse: mfalse}
}

// Set sets a value in a specified column, with the given timestamp.
// The timestamp will be truncated to millisecond granularity.
// A timestamp of ServerTime means to use the server timestamp.
func (m *Mutation) Set(family, column string, ts Timestamp, value []byte) {
	m.ops = append(m.ops, &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName:      family,
		ColumnQualifier: []byte(column),
		TimestampMicros: int64(ts.TruncateToMilliseconds()),
		Value:           value,
	}}})
}

// DeleteCellsInColumn will delete all the cells whose columns are family:column.
func (m *Mutation) DeleteCellsInColumn(family, column string) {
	m.ops = append(m.ops, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
		FamilyName:      family,
		ColumnQualifier: []byte(column),
	}}})
}

// DeleteTimestampRange deletes all cells whose columns are family:column
// and whose timestamps are in the half-open interval [start, end).
// If end is zero, it will be interpreted as infinity.
// The timestamps will be truncated to millisecond granularity.
func (m *Mutation) DeleteTimestampRange(family, column string, start, end Timestamp) {
	m.ops = append(m.ops, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
		FamilyName:      family,
		ColumnQualifier: []byte(column),
		TimeRange: &btpb.TimestampRange{
			StartTimestampMicros: int64(start.TruncateToMilliseconds()),
			EndTimestampMicros:   int64(end.TruncateToMilliseconds()),
		},
	}}})
}

// DeleteCellsInFamily will delete all the cells whose columns are family:*.
func (m *Mutation) DeleteCellsInFamily(family string) {
	m.ops = append(m.ops, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{
		FamilyName: family,
	}}})
}

// DeleteRow deletes the entire row.
func (m *Mutation) DeleteRow() {
	m.ops = append(m.ops, &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromRow_{DeleteFromRow: &btpb.Mutation_DeleteFromRow{}}})
}

// entryErr is a container that combines an entry with the error that was returned for it.
// Err may be nil if no error was returned for the Entry, or if the Entry has not yet been processed.
type entryErr struct {
	Entry *btpb.MutateRowsRequest_Entry
	Err   error
}

// ApplyBulk applies multiple Mutations, up to a maximum of 100,000.
// Each mutation is individually applied atomically,
// but the set of mutations may be applied in any order.
//
// Two types of failures may occur. If the entire process
// fails, (nil, err) will be returned. If specific mutations
// fail to apply, ([]err, nil) will be returned, and the errors
// will correspond to the relevant rowKeys/muts arguments.
//
// Conditional mutations cannot be applied in bulk and providing one will result in an error.
func (t *Table) ApplyBulk(ctx context.Context, rowKeys []string, muts []*Mutation, opts ...ApplyOption) (errs []error, err error) {
	ctx = mergeOutgoingMetadata(ctx, t.md)
	ctx = trace.StartSpan(ctx, "cloud.google.com/go/bigtable/ApplyBulk")
	defer func() { trace.EndSpan(ctx, err) }()

	if len(rowKeys) != len(muts) {
		return nil, fmt.Errorf("mismatched rowKeys and mutation array lengths: %d, %d", len(rowKeys), len(muts))
	}

	origEntries := make([]*entryErr, len(rowKeys))
	for i, key := range rowKeys {
		mut := muts[i]
		if mut.cond != nil {
			return nil, errors.New("conditional mutations cannot be applied in bulk")
		}
		origEntries[i] = &entryErr{Entry: &btpb.MutateRowsRequest_Entry{RowKey: []byte(key), Mutations: mut.ops}}
	}

	for _, group := range groupEntries(origEntries, maxMutations) {
		attrMap := make(map[string]interface{})
		err = gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
			attrMap["rowCount"] = len(group)
			trace.TracePrintf(ctx, attrMap, "Row count in ApplyBulk")
			err := t.doApplyBulk(ctx, group, opts...)
			if err != nil {
				// We want to retry the entire request with the current group
				return err
			}
			group = t.getApplyBulkRetries(group)
			if len(group) > 0 && len(idempotentRetryCodes) > 0 {
				// We have at least one mutation that needs to be retried.
				// Return an arbitrary error that is retryable according to callOptions.
				return status.Errorf(idempotentRetryCodes[0], "Synthetic error: partial failure of ApplyBulk")
			}
			return nil
		}, retryOptions...)
		if err != nil {
			return nil, err
		}
	}

	// All the errors are accumulated into an array and returned, interspersed with nils for successful
	// entries. The absence of any errors means we should return nil.
	var foundErr bool
	for _, entry := range origEntries {
		if entry.Err != nil {
			foundErr = true
		}
		errs = append(errs, entry.Err)
	}
	if foundErr {
		return errs, nil
	}
	return nil, nil
}

// getApplyBulkRetries returns the entries that need to be retried
func (t *Table) getApplyBulkRetries(entries []*entryErr) []*entryErr {
	var retryEntries []*entryErr
	for _, entry := range entries {
		err := entry.Err
		if err != nil && isIdempotentRetryCode[status.Code(err)] && mutationsAreRetryable(entry.Entry.Mutations) {
			// There was an error and the entry is retryable.
			retryEntries = append(retryEntries, entry)
		}
	}
	return retryEntries
}

// doApplyBulk does the work of a single ApplyBulk invocation
func (t *Table) doApplyBulk(ctx context.Context, entryErrs []*entryErr, opts ...ApplyOption) error {
	after := func(res proto.Message) {
		for _, o := range opts {
			o.after(res)
		}
	}

	entries := make([]*btpb.MutateRowsRequest_Entry, len(entryErrs))
	for i, entryErr := range entryErrs {
		entries[i] = entryErr.Entry
	}
	req := &btpb.MutateRowsRequest{
		TableName:    t.c.fullTableName(t.table),
		AppProfileId: t.c.appProfile,
		Entries:      entries,
	}
	stream, err := t.c.client.MutateRows(ctx, req)
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		for i, entry := range res.Entries {
			s := entry.Status
			if s.Code == int32(codes.OK) {
				entryErrs[i].Err = nil
			} else {
				entryErrs[i].Err = status.Errorf(codes.Code(s.Code), s.Message)
			}
		}
		after(res)
	}
	return nil
}

// groupEntries groups entries into groups of a specified size without breaking up
// individual entries.
func groupEntries(entries []*entryErr, maxSize int) [][]*entryErr {
	var (
		res   [][]*entryErr
		start int
		gmuts int
	)
	addGroup := func(end int) {
		if end-start > 0 {
			res = append(res, entries[start:end])
			start = end
			gmuts = 0
		}
	}
	for i, e := range entries {
		emuts := len(e.Entry.Mutations)
		if gmuts+emuts > maxSize {
			addGroup(i)
		}
		gmuts += emuts
	}
	addGroup(len(entries))
	return res
}

// Timestamp is in units of microseconds since 1 January 1970.
type Timestamp int64

// ServerTime is a specific Timestamp that may be passed to (*Mutation).Set.
// It indicates that the server's timestamp should be used.
const ServerTime Timestamp = -1

// Time converts a time.Time into a Timestamp.
func Time(t time.Time) Timestamp { return Timestamp(t.UnixNano() / 1e3) }

// Now returns the Timestamp representation of the current time on the client.
func Now() Timestamp { return Time(time.Now()) }

// Time converts a Timestamp into a time.Time.
func (ts Timestamp) Time() time.Time { return time.Unix(int64(ts)/1e6, int64(ts)%1e6*1e3) }

// TruncateToMilliseconds truncates a Timestamp to millisecond granularity,
// which is currently the only granularity supported.
func (ts Timestamp) TruncateToMilliseconds() Timestamp {
	if ts == ServerTime {
		return ts
	}
	return ts - ts%1000
}

// ApplyReadModifyWrite applies a ReadModifyWrite to a specific row.
// It returns the newly written cells.
func (t *Table) ApplyReadModifyWrite(ctx context.Context, row string, m *ReadModifyWrite) (Row, error) {
	ctx = mergeOutgoingMetadata(ctx, t.md)
	req := &btpb.ReadModifyWriteRowRequest{
		TableName:    t.c.fullTableName(t.table),
		AppProfileId: t.c.appProfile,
		RowKey:       []byte(row),
		Rules:        m.ops,
	}
	res, err := t.c.client.ReadModifyWriteRow(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.Row == nil {
		return nil, errors.New("unable to apply ReadModifyWrite: res.Row=nil")
	}
	r := make(Row)
	for _, fam := range res.Row.Families { // res is *btpb.Row, fam is *btpb.Family
		decodeFamilyProto(r, row, fam)
	}
	return r, nil
}

// ReadModifyWrite represents a set of operations on a single row of a table.
// It is like Mutation but for non-idempotent changes.
// When applied, these operations operate on the latest values of the row's cells,
// and result in a new value being written to the relevant cell with a timestamp
// that is max(existing timestamp, current server time).
//
// The application of a ReadModifyWrite is atomic; concurrent ReadModifyWrites will
// be executed serially by the server.
type ReadModifyWrite struct {
	ops []*btpb.ReadModifyWriteRule
}

// NewReadModifyWrite returns a new ReadModifyWrite.
func NewReadModifyWrite() *ReadModifyWrite { return new(ReadModifyWrite) }

// AppendValue appends a value to a specific cell's value.
// If the cell is unset, it will be treated as an empty value.
func (m *ReadModifyWrite) AppendValue(family, column string, v []byte) {
	m.ops = append(m.ops, &btpb.ReadModifyWriteRule{
		FamilyName:      family,
		ColumnQualifier: []byte(column),
		Rule:            &btpb.ReadModifyWriteRule_AppendValue{AppendValue: v},
	})
}

// Increment interprets the value in a specific cell as a 64-bit big-endian signed integer,
// and adds a value to it. If the cell is unset, it will be treated as zero.
// If the cell is set and is not an 8-byte value, the entire ApplyReadModifyWrite
// operation will fail.
func (m *ReadModifyWrite) Increment(family, column string, delta int64) {
	m.ops = append(m.ops, &btpb.ReadModifyWriteRule{
		FamilyName:      family,
		ColumnQualifier: []byte(column),
		Rule:            &btpb.ReadModifyWriteRule_IncrementAmount{IncrementAmount: delta},
	})
}

// SampleRowKeys returns a sample of row keys in the table. The returned row keys will delimit contiguous sections of
// the table of approximately equal size, which can be used to break up the data for distributed tasks like mapreduces.
func (t *Table) SampleRowKeys(ctx context.Context) ([]string, error) {
	ctx = mergeOutgoingMetadata(ctx, t.md)
	var sampledRowKeys []string
	err := gax.Invoke(ctx, func(ctx context.Context, _ gax.CallSettings) error {
		sampledRowKeys = nil
		req := &btpb.SampleRowKeysRequest{
			TableName:    t.c.fullTableName(t.table),
			AppProfileId: t.c.appProfile,
		}
		ctx, cancel := context.WithCancel(ctx) // for aborting the stream
		defer cancel()

		stream, err := t.c.client.SampleRowKeys(ctx, req)
		if err != nil {
			return err
		}
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			key := string(res.RowKey)
			if key == "" {
				continue
			}

			sampledRowKeys = append(sampledRowKeys, key)
		}
		return nil
	}, retryOptions...)
	return sampledRowKeys, err
}
