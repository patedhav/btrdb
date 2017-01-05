package httpinterface

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/SoftwareDefinedBuildings/btrdb"
	"github.com/SoftwareDefinedBuildings/btrdb/qtree"
	"github.com/pborman/uuid"
)

/*
 * There are two types of CSV requests that have fairly different handling.
 *
 * The first is implemented in request_get_CSV which essentially performs
 * a statistical query of a single stream and returns the results as a
 * CSV with no additional processing.
 *
 * The second one, which is used by the plotter, is implemented in
 * request_post_MULTICSV_IMPL. What this does is perform statistical
 * requests across multiple streams and merge the results together into
 * a single CSV. This is documented in more detail in the body of the method
 * below.
 */
type multi_csv_req struct {
	UUIDS      []string
	Labels     []string
	StartTime  int64
	EndTime    int64
	UnitofTime string
	PointWidth int
}

type aligned_csv_req struct {
	UUIDS       []string
	Labels      []string
	StartTime   int64
	EndTime     int64
	UnitofTime  string
	WindowWidth int64
}

func request_post_WRAPPED_MULTICSV(q *btrdb.Quasar, w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&outstandingHttpReqs, 1)
	defer func() {
		atomic.AddInt32(&outstandingHttpReqs, -1)
	}()
	r.ParseForm()
	bdy := bytes.NewBufferString(r.Form.Get("body"))
	request_post_MULTICSV_IMPL(q, w, bdy, r)
}
func request_post_MULTICSV(q *btrdb.Quasar, w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&outstandingHttpReqs, 1)
	defer func() {
		atomic.AddInt32(&outstandingHttpReqs, -1)
	}()
	request_post_MULTICSV_IMPL(q, w, r.Body, r)
}

func request_post_ALIGNEDCSV(q *btrdb.Quasar, w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&outstandingHttpReqs, 1)
	defer func() {
		atomic.AddInt32(&outstandingHttpReqs, -1)
	}()
	request_post_MULTI_ALIGNED_CSV_IMPL(q, w, r.Body, r)
}

// This is the new CSV query method used by the plotter. It uses aligned windows
func request_post_MULTI_ALIGNED_CSV_IMPL(q *btrdb.Quasar, w http.ResponseWriter, bdy io.Reader, r *http.Request) {
	// The CSV is generated by posting a JSON document containing the parameters.
	// This follows the struct aligned_csv_req above, and would look, for example,
	// like
	// {
	//    "UUIDS": ["bac15d87-17c9-492c-b60b-1bb3d3b1450a", "134acaca-dad3-48fc-b7c4-0aafdb7400f2"],
	//    "Labels": ["C1MAG", "C1ANG"],
	//    "StartTime": 1477187481140869000,
	//    "EndTime": 1477287481140869000,
	//    "UnitofTime": "ns",
	//    "WindowWidth": 60000000000
	// }
	// which uses 1 minute (exactly) windows
	// Parse the request
	dec := json.NewDecoder(bdy)
	req := aligned_csv_req{}
	err := dec.Decode(&req)
	fmt.Println("start1")
	if err != nil {
		doError(w, "bad request")
		return
	}
	if len(req.UUIDS) != len(req.Labels) {
		doError(w, "UUIDS and Labels must be the same length")
		return
	}
	uids := make([]uuid.UUID, len(req.UUIDS))
	for i := 0; i < len(uids); i++ {
		uids[i] = uuid.Parse(req.UUIDS[i])
		if uids[i] == nil {
			doError(w, "UUID "+string(i)+" is malformed")
			return
		}
	}
	// Although we have logic for parsing other time bases,
	// this only affects how the start time and end time (and window width)
	// are interpreted, not the actual returned data
	unitoftime := req.UnitofTime
	divisor := int64(1)
	switch unitoftime {
	case "":
		fallthrough
	case "ms":
		divisor = 1000000 //ns to ms
	case "ns":
		divisor = 1
	case "us":
		divisor = 1000 //ns to us
	case "s":
		divisor = 1000000000 //ns to s
	default:
		doError(w, "unitoftime must be 'ns', 'ms', 'us' or 's'")
		return
	}
	if req.StartTime >= btrdb.MaximumTime/divisor ||
		req.StartTime <= btrdb.MinimumTime/divisor {
		doError(w, "start time out of bounds")
		return
	}
	if req.EndTime >= btrdb.MaximumTime/divisor ||
		req.EndTime <= btrdb.MinimumTime/divisor {
		doError(w, "end time out of bounds")
		return
	}
	st := req.StartTime * divisor
	et := req.EndTime * divisor
	wt := req.WindowWidth
	if wt < 0 || wt > 365*24*60*60*1000*1000*1000 {
		doError(w, fmt.Sprintf("WindowWidth must be less than a year, got %d ns", wt))
		return
	}
	if st+wt > et {
		doError(w, "Your window is bigger than your query range")
		return
	}
	//These will be the channels containing each of the streams we
	//are merging
	chanVs := make([]chan qtree.StatRecord, len(uids))
	chanBad := make([]bool, len(uids))
	chanHead := make([]qtree.StatRecord, len(uids))
	//We query all of them
	for i := 0; i < len(uids); i++ {
		logh("QWVS", fmt.Sprintf("u=%v st=%v et=%v wt=%v", uids[i].String(), st, et, wt), r)
		var gen uint64
		chanVs[i], gen = q.QueryWindow(uids[i], st, et, btrdb.LatestGeneration, uint64(wt), 0)
		if gen == 0 {
			doError(w, fmt.Sprintf("Stream %s does not exist", uids[i]))
			return
		}
	}
	//The reload function repopulates some data, checking for errors.
	reload := func(c int) {
		select {
		case v, ok := <-chanVs[c]:
			if ok {
				chanHead[c] = v
			} else {
				chanBad[c] = true
			}
		}
	}
	//emit will put out a section of data
	emit := func(r qtree.StatRecord) {
		w.Write([]byte(fmt.Sprintf(",%d,%f,%f,%f", r.Count, r.Min, r.Mean, r.Max)))
	}
	//emitb will put out blank data (if the stream is missing data for example)
	emitb := func() {
		w.Write([]byte(",,,,"))
	}
	//emitt will put the time, which always goes at the front of the row.
	//this is what you should customise if you want multiple time columns
	//with different formats. Make sure you customise the headings below
	//too
	var excelFmt = "2006-01-02 15:04:05.000"
	emitt := func(t int64) {
		w.Write([]byte(fmt.Sprintf("%d,%s", t, time.Unix(0, t).Format(excelFmt))))
	}
	emitnl := func() {
		w.Write([]byte("\n"))
	}
	//Prime the first results
	for i := 0; i < len(uids); i++ {
		reload(i)
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\"quasar_results.csv\"")
	//Print the headers
	w.Write([]byte("Time[ns],Time"))

	//And other time headings
	for i := 0; i < len(uids); i++ {
		w.Write([]byte(fmt.Sprintf(",%s(cnt),%s(min),%s(mean),%s(max)",
			req.Labels[i],
			req.Labels[i],
			req.Labels[i],
			req.Labels[i])))
	}
	w.Write([]byte("\n"))

	//Now merge out the results
	for t := st; t < et; t += wt {
		//First locate the min time
		minset := false
		min := int64(0)
		for i := 0; i < len(uids); i++ {
			for !chanBad[i] && chanHead[i].Time < t {
				log.Warning("discarding duplicate time %v:%v", i, chanHead[i].Time)
				reload(i)
			}
			if !chanBad[i] && (!minset || chanHead[i].Time < min) {
				minset = true
				min = chanHead[i].Time
			}
		}
		if minset == false {
			//We are done. There are no more live streams
			return
		}
		//If the min time is later than t, emit blank lines until we catch up
		for ; t < min; t += wt {
			emitt(t)
			emitnl()
		}
		if t != min {
			log.Panic("critical error")
		}
		//Now emit all values at that time
		emitt(t)
		for i := 0; i < len(uids); i++ {
			if !chanBad[i] && chanHead[i].Time == min {
				emit(chanHead[i])
				reload(i)
			} else {
				emitb()
			}
		}
		emitnl()
	}
}

// This is the main CSV query method used by the plotter
func request_post_MULTICSV_IMPL(q *btrdb.Quasar, w http.ResponseWriter, bdy io.Reader, r *http.Request) {
	// The CSV is generated by posting a JSON document containing the parameters.
	// This follows the struct multi_csv_req above, and would look, for example,
	// like
	// {
	//    "UUIDS": ["bac15d87-17c9-492c-b60b-1bb3d3b1450a", "134acaca-dad3-48fc-b7c4-0aafdb7400f2"],
	//    "Labels": ["C1MAG", "C1ANG"],
	//    "StartTime": 1477187481140869000,
	//    "EndTime": 1477287481140869000,
	//    "UnitofTime": "ns",
	//    "PointWidth": 34
	// }

	// Parse the request
	dec := json.NewDecoder(bdy)
	req := multi_csv_req{}
	err := dec.Decode(&req)
	if err != nil {
		doError(w, "bad request")
		return
	}
	if len(req.UUIDS) != len(req.Labels) {
		doError(w, "UUIDS and Labels must be the same length")
		return
	}
	uids := make([]uuid.UUID, len(req.UUIDS))
	for i := 0; i < len(uids); i++ {
		uids[i] = uuid.Parse(req.UUIDS[i])
		if uids[i] == nil {
			doError(w, "UUID "+string(i)+" is malformed")
			return
		}
	}
	// Although we have logic for parsing other time bases,
	// this only affects how the start time and end time
	// are interpreted, not the actual returned data
	unitoftime := req.UnitofTime
	divisor := int64(1)
	switch unitoftime {
	case "":
		fallthrough
	case "ms":
		divisor = 1000000 //ns to ms
	case "ns":
		divisor = 1
	case "us":
		divisor = 1000 //ns to us
	case "s":
		divisor = 1000000000 //ns to s
	default:
		doError(w, "unitoftime must be 'ns', 'ms', 'us' or 's'")
		return
	}
	if req.StartTime >= btrdb.MaximumTime/divisor ||
		req.StartTime <= btrdb.MinimumTime/divisor {
		doError(w, "start time out of bounds")
		return
	}
	if req.EndTime >= btrdb.MaximumTime/divisor ||
		req.EndTime <= btrdb.MinimumTime/divisor {
		doError(w, "end time out of bounds")
		return
	}
	st := req.StartTime * divisor
	et := req.EndTime * divisor
	if req.PointWidth < 0 || req.PointWidth >= 63 {
		doError(w, "PointWidth must be between 0 and 63")
		return
	}
	pw := uint8(req.PointWidth)
	//These will be the channels containing each of the streams we
	//are merging
	chanVs := make([]chan qtree.StatRecord, len(uids))
	chanEs := make([]chan error, len(uids))
	chanBad := make([]bool, len(uids))
	chanHead := make([]qtree.StatRecord, len(uids))
	//We query all of them
	for i := 0; i < len(uids); i++ {
		logh("QSVS", fmt.Sprintf("u=%v st=%v et=%v pw=%v", uids[i].String(), st, et, pw), r)
		chanVs[i], chanEs[i], _ = q.QueryStatisticalValuesStream(uids[i], st, et, btrdb.LatestGeneration, pw)
	}
	//The reload function repopulates some data, checking for errors.
	reload := func(c int) {
		select {
		case v, ok := <-chanVs[c]:
			if ok {
				chanHead[c] = v
			} else {
				chanBad[c] = true
			}
		case e, ok := <-chanEs[c]:
			if ok {
				log.Critical("MultiCSV error: ", e)
				chanBad[c] = true
			}
		}
	}
	//emit will put out a section of data
	emit := func(r qtree.StatRecord) {
		w.Write([]byte(fmt.Sprintf(",%d,%f,%f,%f", r.Count, r.Min, r.Mean, r.Max)))
	}
	//emitb will put out blank data (if the stream is missing data for example)
	emitb := func() {
		w.Write([]byte(",,,,"))
	}
	//emitt will put the time, which always goes at the front of the row.
	//this is what you should customise if you want multiple time columns
	//with different formats. Make sure you customise the headings below
	//too
	var excelFmt = "2006-01-02 15:04:05.000"
	emitt := func(t int64) {
		w.Write([]byte(fmt.Sprintf("%d,%s", t, time.Unix(0, t).Format(excelFmt))))
	}
	emitnl := func() {
		w.Write([]byte("\n"))
	}
	//Prime the first results
	for i := 0; i < len(uids); i++ {
		reload(i)
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\"quasar_results.csv\"")
	//Print the headers
	w.Write([]byte("Time[ns],Time"))

	//And other time headings
	for i := 0; i < len(uids); i++ {
		w.Write([]byte(fmt.Sprintf(",%s(cnt),%s(min),%s(mean),%s(max)",
			req.Labels[i],
			req.Labels[i],
			req.Labels[i],
			req.Labels[i])))
	}
	w.Write([]byte("\n"))

	//Now merge out the results
	st = st &^ ((1 << pw) - 1)
	for t := st; t < et; t += (1 << pw) {
		//First locate the min time
		minset := false
		min := int64(0)
		for i := 0; i < len(uids); i++ {
			for !chanBad[i] && chanHead[i].Time < t {
				log.Warning("discarding duplicate time %v:%v", i, chanHead[i].Time)
				reload(i)
			}
			if !chanBad[i] && (!minset || chanHead[i].Time < min) {
				minset = true
				min = chanHead[i].Time
			}
		}
		if minset == false {
			//We are done. There are no more live streams
			return
		}
		//If the min time is later than t, emit blank lines until we catch up
		for ; t < min; t += (1 << pw) {
			emitt(t)
			emitnl()
		}
		if t != min {
			log.Critical("WTF t=%v, min=%v, pw=%v, dt=%v, dm=%v delte=%v",
				t, min, 1<<pw, t&((1<<pw)-1), min&((1<<pw)-1), min-t)
			log.Panic("critical error")
		}
		//Now emit all values at that time
		emitt(t)
		for i := 0; i < len(uids); i++ {
			if !chanBad[i] && chanHead[i].Time == min {
				emit(chanHead[i])
				reload(i)
			} else {
				emitb()
			}
		}
		emitnl()
	}
}

func request_get_CSV(q *btrdb.Quasar, w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&outstandingHttpReqs, 1)
	defer func() {
		atomic.AddInt32(&outstandingHttpReqs, -1)
	}()
	r.ParseForm()
	ids := r.Form.Get(":uuid")
	id := uuid.Parse(ids)
	if id == nil {
		log.Critical("ids: '%v'", ids)
		doError(w, "malformed uuid")
		return
	}
	st, ok, msg := parseInt(r.Form.Get("starttime"), -(16 << 56), (48 << 56))
	if !ok {
		doError(w, "bad start time: "+msg)
		return
	}
	et, ok, msg := parseInt(r.Form.Get("endtime"), -(16 << 56), (48 << 56))
	if !ok {
		doError(w, "bad end time: "+msg)
		return
	}
	if et <= st {
		doError(w, "end time <= start time")
		return
	}
	versions := r.Form.Get("ver")
	if versions == "" {
		versions = "0"
	}
	//Technically this is incorrect, but I doubt we will overflow this
	versioni, ok, msg := parseInt(versions, 0, 1<<63-1)
	version := uint64(versioni)
	if !ok {
		doError(w, "malformed version: "+msg)
		return
	}
	if version == 0 {
		version = btrdb.LatestGeneration
	}
	unitoftime := r.Form.Get("unitoftime")
	divisor := int64(1)
	switch unitoftime {
	case "":
		fallthrough
	case "ms":
		divisor = 1000000 //ns to ms
	case "ns":
		divisor = 1
	case "us":
		divisor = 1000 //ns to us
	case "s":
		divisor = 1000000000 //ns to s
	default:
		doError(w, "unitoftime must be 'ns', 'ms', 'us' or 's'")
		return
	}
	if st >= btrdb.MaximumTime/divisor ||
		st <= btrdb.MinimumTime/divisor {
		doError(w, "start time out of bounds")
		return
	}
	if et >= btrdb.MaximumTime/divisor ||
		et <= btrdb.MinimumTime/divisor {
		doError(w, "end time out of bounds")
		return
	}
	st *= divisor
	et *= divisor
	pws := r.Form.Get("pw")
	pw := uint8(0)
	if pws != "" {
		pwl, ok, msg := parseInt(pws, 0, 63)
		if !ok {
			doError(w, "bad point width: "+msg)
			return
		}
		if divisor != 1 {
			doError(w, "statistical results require unitoftime=ns")
			return
		}
		pw = uint8(pwl)
	}
	logh("QSVSn", fmt.Sprintf("u=%s st=%v et=%v pw=%v", id.String(), st, et, pw), r)
	rvchan, echan, _ := q.QueryStatisticalValuesStream(id, st, et, version, pw)
	w.WriteHeader(200)
	w.Write([]byte("Time[ns],Mean,Min,Max,Count\n"))
	for {
		select {
		case v, ok := <-rvchan:
			if ok {
				w.Write([]byte(fmt.Sprintf("%d,%f,%f,%f,%d\n", v.Time, v.Mean, v.Min, v.Max, v.Count)))
			} else {
				//Done
				return
			}

		case err, ok := <-echan:
			if ok {
				w.Write([]byte(fmt.Sprintf("!ABORT ERROR: %v", err)))
				return
			}
		}
	}
	return
}