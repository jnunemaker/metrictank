package mdata

import (
	"fmt"
	"github.com/raintank/met/helper"
	"testing"
)

var dnstore = NewDevnullStore()

type point struct {
	ts  uint32
	val float64
}

func (p point) String() string {
	return fmt.Sprintf("point{%0.f at %d}", p.val, p.ts)
}

type Checker struct {
	t      *testing.T
	agg    *AggMetric
	points []point
}

func NewChecker(t *testing.T, agg *AggMetric) *Checker {
	return &Checker{t, agg, make([]point, 0)}
}

// always add points in ascending order, never same ts!
func (c *Checker) Add(ts uint32, val float64) {
	c.agg.Add(ts, val)
	c.points = append(c.points, point{ts, val})
}

// from to is the range that gets requested from AggMetric
// first/last is what we use as data range to compare to (both inclusive)
// these may be different because AggMetric returns broader rangers (due to packed format),
func (c *Checker) Verify(primary bool, from, to, first, last uint32) {
	currentClusterStatus := CluStatus.IsPrimary()
	CluStatus.Set(primary)
	_, iters := c.agg.Get(from, to)
	// we don't do checking or fancy logic, it is assumed that the caller made sure first and last are ts of actual points
	var pi int // index of first point we want
	var pj int // index of last point we want
	for pi = 0; c.points[pi].ts != first; pi++ {
	}
	for pj = pi; c.points[pj].ts != last; pj++ {
	}
	c.t.Logf("verifying AggMetric.Get(%d,%d) =?= %d <= ts <= %d", from, to, first, last)
	index := pi - 1
	for _, iter := range iters {
		for iter.Next() {
			index++
			tt, vv := iter.Values()
			//c.t.Logf("got (%v,%v).. should be (%v,%v)", tt, vv, c.points[index].ts, c.points[index].val)
			if index > pj {
				c.t.Fatalf("Values()=(%v,%v), want end of stream\n", tt, vv)
			}
			if c.points[index].ts != tt || c.points[index].val != vv {
				c.t.Fatalf("Values()=(%v,%v), want (%v,%v)\n", tt, vv, c.points[index].ts, c.points[index].val)
			}
		}
	}
	if index != pj {
		c.t.Fatalf("not all values returned. missing %v", c.points[index:pj+1])
	}
	CluStatus.Set(currentClusterStatus)
}

func TestAggMetric(t *testing.T) {
	stats, _ := helper.New(false, "", "standard", "metrictank", "")
	CluStatus = NewClusterStatus("default", false)
	InitMetrics(stats)

	c := NewChecker(t, NewAggMetric(dnstore, "foo", 100, 5, 1, []AggSetting{}...))

	// basic case, single range
	c.Add(101, 101)
	c.Verify(true, 100, 200, 101, 101)
	c.Add(105, 105)
	c.Verify(true, 100, 199, 101, 105)
	c.Add(115, 115)
	c.Add(125, 125)
	c.Add(135, 135)
	c.Verify(true, 100, 199, 101, 135)

	// add new ranges, aligned and unaligned
	c.Add(200, 200)
	c.Add(315, 315)
	c.Verify(true, 100, 399, 101, 315)

	// verify as secondary node. Data from the first chunk should not be returned.
	c.Verify(false, 100, 399, 200, 315)

	// get subranges
	c.Verify(true, 120, 299, 101, 200)
	c.Verify(true, 220, 299, 200, 200)
	c.Verify(true, 312, 330, 315, 315)

	// border dancing. good for testing inclusivity and exclusivity
	c.Verify(true, 100, 199, 101, 135)
	c.Verify(true, 100, 200, 101, 135)
	c.Verify(true, 100, 201, 101, 200)
	c.Verify(true, 198, 199, 101, 135)
	c.Verify(true, 199, 200, 101, 135)
	c.Verify(true, 200, 201, 200, 200)
	c.Verify(true, 201, 202, 200, 200)
	c.Verify(true, 299, 300, 200, 200)
	c.Verify(true, 300, 301, 315, 315)

	// skipping
	c.Add(510, 510)
	c.Add(512, 512)
	c.Verify(true, 100, 599, 101, 512)

	// basic wraparound
	c.Add(610, 610)
	c.Add(612, 612)
	c.Add(710, 710)
	c.Add(712, 712)
	// TODO would be nice to test that it panics when requesting old range. something with recover?
	//c.Verify(true, 100, 799, 101, 512)

	// largest range we have so far
	c.Verify(true, 300, 799, 315, 712)
	// a smaller range
	c.Verify(true, 502, 799, 510, 712)

	// the circular buffer had these ranges:
	// 100 200 300 skipped 500
	// then we made it:
	// 600 700 300 skipped 500
	// now we want to do another wrap around with skip (must have cleared old data)
	// let's jump to 1200. the accessible range should then be 800-1200
	// clea 1200 clea clea clea
	// we can't (and shouldn't, due to abstraction) test the clearing itself
	// but we just check we only get this point
	c.Add(1299, 1299)
	// TODO: implement skips and enable this
	//	c.Verify(true, 800, 1299, 1299, 1299)
}

// basic expected RAM usage for 1 iteration (= 1 days)
// 1000 metrics * (3600 * 24 / 10 ) points per metric * 1.3 B/point = 11 MB
// 1000 metrics * 5 agg metrics per metric * (3600 * 24 / 300) points per aggmetric * 1.3B/point = 1.9 MB
// total -> 13 MB
// go test -run=XX -bench=Bench -benchmem -v -memprofile mem.out
// go tool pprof -inuse_space metrictank.test mem.out -> shows 25 MB in use

// TODO update once we clean old data, then we should look at numChunks
func BenchmarkAggMetrics1000Metrics1Day(b *testing.B) {
	stats, _ := helper.New(false, "", "standard", "metrictank", "")
	InitMetrics(stats)
	CluStatus = NewClusterStatus("default", false)
	// we will store 10s metrics in 5 chunks of 2 hours
	// aggragate them in 5min buckets, stored in 1 chunk of 24hours
	chunkSpan := uint32(2 * 3600)
	numChunks := uint32(5)
	chunkMaxStale := uint32(3600)
	metricMaxStale := uint32(21600)
	ttl := uint32(84600)
	aggSettings := []AggSetting{
		{
			Span:      uint32(300),
			ChunkSpan: uint32(24 * 3600),
			NumChunks: uint32(1),
		},
	}

	keys := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		keys[i] = fmt.Sprintf("hello.this.is.a.test.key.%d", i)
	}

	metrics := NewAggMetrics(dnstore, chunkSpan, numChunks, chunkMaxStale, metricMaxStale, ttl, 0, aggSettings)

	maxT := 3600 * 24 * uint32(b.N) // b.N in days
	for t := uint32(1); t < maxT; t += 10 {
		for metricI := 0; metricI < 1000; metricI++ {
			m := metrics.GetOrCreate(keys[metricI])
			m.Add(t, float64(t))
		}
	}
}

func BenchmarkAggMetrics1kSeries2Chunks1kQueueSize(b *testing.B) {
	stats, _ := helper.New(false, "", "standard", "metrictank", "")
	InitMetrics(stats)

	chunkSpan := uint32(600)
	numChunks := uint32(5)
	chunkMaxStale := uint32(3600)
	metricMaxStale := uint32(21600)

	CluStatus = NewClusterStatus("default", true)

	ttl := uint32(84600)
	aggSettings := []AggSetting{
		{
			Span:      uint32(300),
			ChunkSpan: uint32(24 * 3600),
			NumChunks: uint32(2),
		},
	}

	keys := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		keys[i] = fmt.Sprintf("hello.this.is.a.test.key.%d", i)
	}

	metrics := NewAggMetrics(dnstore, chunkSpan, numChunks, chunkMaxStale, metricMaxStale, ttl, 0, aggSettings)

	maxT := uint32(1200)
	for t := uint32(1); t < maxT; t += 10 {
		for metricI := 0; metricI < 1000; metricI++ {
			m := metrics.GetOrCreate(keys[metricI])
			m.Add(t, float64(t))
		}
	}
}

func BenchmarkAggMetrics10kSeries2Chunks10kQueueSize(b *testing.B) {
	stats, _ := helper.New(false, "", "standard", "metrictank", "")
	InitMetrics(stats)

	chunkSpan := uint32(600)
	numChunks := uint32(5)
	chunkMaxStale := uint32(3600)
	metricMaxStale := uint32(21600)

	CluStatus = NewClusterStatus("default", true)

	ttl := uint32(84600)
	aggSettings := []AggSetting{
		{
			Span:      uint32(300),
			ChunkSpan: uint32(24 * 3600),
			NumChunks: uint32(2),
		},
	}

	keys := make([]string, 10000)
	for i := 0; i < 10000; i++ {
		keys[i] = fmt.Sprintf("hello.this.is.a.test.key.%d", i)
	}

	metrics := NewAggMetrics(dnstore, chunkSpan, numChunks, chunkMaxStale, metricMaxStale, ttl, 0, aggSettings)

	maxT := uint32(1200)
	for t := uint32(1); t < maxT; t += 10 {
		for metricI := 0; metricI < 10000; metricI++ {
			m := metrics.GetOrCreate(keys[metricI])
			m.Add(t, float64(t))
		}
	}
}

func BenchmarkAggMetrics100kSeries2Chunks100kQueueSize(b *testing.B) {
	stats, _ := helper.New(false, "", "standard", "metrictank", "")
	InitMetrics(stats)

	chunkSpan := uint32(600)
	numChunks := uint32(5)
	chunkMaxStale := uint32(3600)
	metricMaxStale := uint32(21600)

	CluStatus = NewClusterStatus("default", true)

	ttl := uint32(84600)
	aggSettings := []AggSetting{
		{
			Span:      uint32(300),
			ChunkSpan: uint32(24 * 3600),
			NumChunks: uint32(2),
		},
	}

	keys := make([]string, 100000)
	for i := 0; i < 100000; i++ {
		keys[i] = fmt.Sprintf("hello.this.is.a.test.key.%d", i)
	}

	metrics := NewAggMetrics(dnstore, chunkSpan, numChunks, chunkMaxStale, metricMaxStale, ttl, 0, aggSettings)

	maxT := uint32(1200)
	for t := uint32(1); t < maxT; t += 10 {
		for metricI := 0; metricI < 100000; metricI++ {
			m := metrics.GetOrCreate(keys[metricI])
			m.Add(t, float64(t))
		}
	}
}
