package integrations

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"testing"
	"text/template"
	"time"

	"github.com/influxdb/influxdb/client"
	imodels "github.com/influxdb/influxdb/models"
	"github.com/influxdb/kapacitor"
	"github.com/influxdb/kapacitor/clock"
	"github.com/influxdb/kapacitor/services/httpd"
	"github.com/influxdb/kapacitor/wlog"
	"github.com/stretchr/testify/assert"
)

var httpService *httpd.Service

func init() {
	wlog.LogLevel = wlog.OFF
	// create API server
	httpService = httpd.NewService(httpd.NewConfig())
	err := httpService.Open()
	if err != nil {
		panic(err)
	}
}

func TestStream_Window(t *testing.T) {
	assert := assert.New(t)

	var script = `
stream
	.from('''"dbname"."rpname"."cpu"''')
	.where("host = 'serverA'")
	.window()
		.period(10s)
		.every(10s)
	.httpOut("TestStream_Window")
`

	nums := []float64{
		97.1,
		92.6,
		95.6,
		93.1,
		92.6,
		95.8,
		92.7,
		96.0,
		93.4,
		95.3,
	}

	values := make([][]interface{}, len(nums))
	for i, num := range nums {
		values[i] = []interface{}{
			time.Date(1970, 1, 1, 0, 0, i, 0, time.UTC),
			num,
		}
	}

	er := kapacitor.Result{
		Series: imodels.Rows{
			{
				Name:    "cpu",
				Tags:    nil,
				Columns: []string{"time", "value"},
				Values:  values,
			},
		},
	}

	clock, et, errCh, tm := testStreamer(t, "TestStream_Window", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Get the result
	output, err := et.GetOutput("TestStream_Window")
	if !assert.Nil(err) {
		t.FailNow()
	}

	resp, err := http.Get(output.Endpoint())
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Assert we got the expected result
	result := kapacitor.ResultFromJSON(resp.Body)
	if eq, msg := compareResults(er, result); !eq {
		t.Error(msg)
	}
}

func TestStream_SimpleMR(t *testing.T) {
	assert := assert.New(t)

	var script = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(10s)
		.every(10s)
	.mapReduce(influxql.count("value"))
	.httpOut("TestStream_SimpleMR")
`
	er := kapacitor.Result{
		Series: imodels.Rows{
			{
				Name:    "cpu",
				Tags:    nil,
				Columns: []string{"time", "count"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					10.0,
				}},
			},
		},
	}

	clock, et, errCh, tm := testStreamer(t, "TestStream_SimpleMR", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(15 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Get the result
	output, err := et.GetOutput("TestStream_SimpleMR")
	if !assert.Nil(err) {
		t.FailNow()
	}

	resp, err := http.Get(output.Endpoint())
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Assert we got the expected result
	result := kapacitor.ResultFromJSON(resp.Body)
	if eq, msg := compareResults(er, result); !eq {
		t.Error(msg)
	}
}

func TestStream_GroupBy(t *testing.T) {
	assert := assert.New(t)

	var script = `
stream
	.from("errors")
	.groupBy("service")
	.window()
		.period(10s)
		.every(10s)
	.mapReduce(influxql.sum("value"))
	.httpOut("error_count")
`

	er := kapacitor.Result{
		Series: imodels.Rows{
			{
				Name:    "errors",
				Tags:    map[string]string{"service": "cartA"},
				Columns: []string{"time", "sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					47.0,
				}},
			},
			{
				Name:    "errors",
				Tags:    map[string]string{"service": "login"},
				Columns: []string{"time", "sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					45.0,
				}},
			},
			{
				Name:    "errors",
				Tags:    map[string]string{"service": "front"},
				Columns: []string{"time", "sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 8, 0, time.UTC),
					32.0,
				}},
			},
		},
	}

	clock, et, errCh, tm := testStreamer(t, "TestStream_GroupBy", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Get the result
	output, err := et.GetOutput("error_count")
	if !assert.Nil(err) {
		t.FailNow()
	}

	resp, err := http.Get(output.Endpoint())
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Assert we got the expected result
	result := kapacitor.ResultFromJSON(resp.Body)
	if eq, msg := compareResults(er, result); !eq {
		t.Error(msg)
	}
}

func TestStream_Join(t *testing.T) {
	assert := assert.New(t)

	var script = `
var errorCounts = stream
			.fork()
			.from("errors")
			.groupBy("service")
			.window()
				.period(10s)
				.every(10s)
			.mapReduce(influxql.sum("value"))

var viewCounts = stream
			.fork()
			.from("views")
			.groupBy("service")
			.window()
				.period(10s)
				.every(10s)
			.mapReduce(influxql.sum("value"))

errorCounts.join(viewCounts)
		.as("errors", "views")
		.rename("error_view")
		.apply(expr("error_percent", "errors.sum / views.sum"))
		.httpOut("error_rate")
`

	er := kapacitor.Result{
		Series: imodels.Rows{
			{
				Name:    "error_view",
				Tags:    map[string]string{"service": "cartA"},
				Columns: []string{"time", "error_percent", "errors.sum", "views.sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					0.01,
					47.0,
					4700.0,
				}},
			},
			{
				Name:    "error_view",
				Tags:    map[string]string{"service": "login"},
				Columns: []string{"time", "error_percent", "errors.sum", "views.sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					0.01,
					45.0,
					4500.0,
				}},
			},
			{
				Name:    "error_view",
				Tags:    map[string]string{"service": "front"},
				Columns: []string{"time", "error_percent", "errors.sum", "views.sum"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 8, 0, time.UTC),
					0.01,
					32.0,
					3200.0,
				}},
			},
		},
	}

	clock, et, errCh, tm := testStreamer(t, "TestStream_Join", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Get the result
	output, err := et.GetOutput("error_rate")
	if !assert.Nil(err) {
		t.FailNow()
	}

	resp, err := http.Get(output.Endpoint())
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Assert we got the expected result
	result := kapacitor.ResultFromJSON(resp.Body)
	if eq, msg := compareResults(er, result); !eq {
		t.Error(msg)
	}
}

func TestStream_Union(t *testing.T) {
	assert := assert.New(t)

	var script = `
var cpu = stream
			.fork()
			.from("cpu")
			.where("cpu = 'total'")
var mem = stream
			.fork()
			.from("memory")
			.where("type = 'free'")
var disk = stream
			.fork()
			.from("disk")
			.where("device = 'sda'")

cpu.union(mem, disk)
		.rename("cpu_mem_disk")
		.window()
			.period(10s)
			.every(10s)
		.mapReduce(influxql.count("value"))
		.httpOut("all")
`

	er := kapacitor.Result{
		Series: imodels.Rows{
			{
				Name:    "cpu_mem_disk",
				Tags:    nil,
				Columns: []string{"time", "count"},
				Values: [][]interface{}{[]interface{}{
					time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
					24.0,
				}},
			},
		},
	}

	clock, et, errCh, tm := testStreamer(t, "TestStream_Union", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(15 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Get the result
	output, err := et.GetOutput("all")
	if !assert.Nil(err) {
		t.FailNow()
	}

	resp, err := http.Get(output.Endpoint())
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Assert we got the expected result
	result := kapacitor.ResultFromJSON(resp.Body)
	if eq, msg := compareResults(er, result); !eq {
		t.Error(msg)
	}
}

func TestStream_Aggregations(t *testing.T) {
	assert := assert.New(t)

	type testCase struct {
		Method string
		Args   string
		ER     kapacitor.Result
	}

	var scriptTmpl = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(10s)
		.every(10s)
	.mapReduce({{ .Method }}("value" {{ .Args }}))
	.httpOut("{{ .Method }}")
`
	testCases := []testCase{
		testCase{
			Method: "influxql.sum",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "sum"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							941.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.count",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "count"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							10.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.distinct",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "distinct"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							[]interface{}{92.0, 93.0, 95.0, 96.0, 98.0},
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.mean",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "mean"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							94.1,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.median",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "median"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							94.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.min",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "min"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							92.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.max",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "max"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							98.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.spread",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "spread"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							6.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.stddev",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "stddev"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							2.0248456731316584,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.first",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "first"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							98.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.last",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "last"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							95.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.percentile",
			Args:   ", 90.0",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "percentile"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							96.0,
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.top",
			Args:   ", 2",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "top"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							[]interface{}{98.0, 96.0},
						}},
					},
				},
			},
		},
		testCase{
			Method: "influxql.bottom",
			Args:   ", 3",
			ER: kapacitor.Result{
				Series: imodels.Rows{
					{
						Name:    "cpu",
						Tags:    nil,
						Columns: []string{"time", "bottom"},
						Values: [][]interface{}{[]interface{}{
							time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC),
							[]interface{}{92.0, 92.0, 92.0},
						}},
					},
				},
			},
		},
	}

	tmpl, err := template.New("script").Parse(scriptTmpl)
	if !assert.Nil(err) {
		t.FailNow()
	}

	for _, tc := range testCases {
		t.Log("Method:", tc.Method)
		var script bytes.Buffer
		tmpl.Execute(&script, tc)
		clock, et, errCh, tm := testStreamer(
			t,
			"TestStream_Aggregations",
			string(script.Bytes()),
		)
		defer tm.Close()

		// Move time forward
		clock.Set(clock.Zero().Add(13 * time.Second))
		// Wait till the replay has finished
		assert.Nil(<-errCh)
		// Wait till the task is finished
		assert.Nil(et.Err())

		// Get the result
		output, err := et.GetOutput(tc.Method)
		if !assert.Nil(err) {
			t.FailNow()
		}

		resp, err := http.Get(output.Endpoint())
		if !assert.Nil(err) {
			t.FailNow()
		}

		// Assert we got the expected result
		result := kapacitor.ResultFromJSON(resp.Body)
		if eq, msg := compareResults(tc.ER, result); !eq {
			t.Error(tc.Method + ": " + msg)
		}
	}
}

func TestStream_CustomFunctions(t *testing.T) {
	var script = `
var fMap = loadMapFunc("./TestCustomMapFunction.py")
var fReduce = loadReduceFunc("./TestCustomReduceFunction.py")
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(1s)
		.every(1s)
	.map(fMap, "idle")
	.reduce(fReduce)
	.cache()
`

	//er := kapacitor.Result{}

	testStreamer(t, "TestStream_CustomFunctions", script)
}

func TestStream_CustomMRFunction(t *testing.T) {
	var script = `
var fMapReduce = loadMapReduceFunc("./TestCustomMapReduceFunction.py")
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(1s)
		.every(1s)
	.mapReduce(fMap, "idle")
	.cache()
`

	//er := kapacitor.Result{}

	testStreamer(t, "TestStream_CustomMRFunction", script)
}

func TestStream_Alert(t *testing.T) {
	assert := assert.New(t)

	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ans, err := ioutil.ReadAll(r.Body)
		if !assert.Nil(err) {
			t.FailNow()
		}
		requestCount++
		expAns := `{"level":"CRITICAL","data":{"Series":[{"name":"cpu","columns":["time","count"],"values":[["1970-01-01T00:00:09Z",10]]}],"Err":null}}`
		assert.Equal(expAns, string(ans))
	}))
	defer ts.Close()

	var script = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(10s)
		.every(10s)
	.mapReduce(influxql.count("idle"))
	.alert()
		.info("count > 6")
		.warn("count > 7")
		.crit("count > 8")
		.post("` + ts.URL + `")
`

	clock, et, errCh, tm := testStreamer(t, "TestStream_Alert", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	assert.Equal(1, requestCount)
}

func TestStream_AlertSigma(t *testing.T) {
	assert := assert.New(t)

	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ans, err := ioutil.ReadAll(r.Body)
		if !assert.Nil(err) {
			t.FailNow()
		}
		requestCount++
		expAns := `{"level":"INFO","data":{"Series":[{"name":"cpu","tags":{"host":"serverA","type":"idle"},"columns":["time","sigma","value"],"values":[["1970-01-01T00:00:07Z",2.469916402324427,16]]}],"Err":null}}`
		assert.Equal(expAns, string(ans))
	}))
	defer ts.Close()

	var script = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.apply(expr("sigma", "sigma(value)"))
	.alert()
		.info("sigma > 2")
		.warn("sigma > 3")
		.crit("sigma > 3.5")
		.post("` + ts.URL + `")
`

	clock, et, errCh, tm := testStreamer(t, "TestStream_AlertSigma", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	assert.Equal(1, requestCount)
}

func TestStream_AlertComplexWhere(t *testing.T) {
	assert := assert.New(t)

	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ans, err := ioutil.ReadAll(r.Body)
		if !assert.Nil(err) {
			t.FailNow()
		}
		requestCount++
		expAns := `{"level":"CRITICAL","data":{"Series":[{"name":"cpu","tags":{"host":"serverA","type":"idle"},"columns":["time","value"],"values":[["1970-01-01T00:00:07Z",16]]}],"Err":null}}`
		assert.Equal(expAns, string(ans))
	}))
	defer ts.Close()

	var script = `
stream
	.from("cpu")
	.where("host = 'serverA' AND sigma(value) > 2")
	.alert()
		.crit("true")
		.post("` + ts.URL + `")
`

	clock, et, errCh, tm := testStreamer(t, "TestStream_AlertComplexWhere", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	assert.Equal(1, requestCount)
}

func TestStream_AlertFlapping(t *testing.T) {
	assert := assert.New(t)

	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
	}))
	defer ts.Close()
	var script = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.alert()
		.info("value < 95")
		.warn("value < 94")
		.crit("value < 93")
		.flapping(0.25, 0.50)
		.post("` + ts.URL + `")
`

	clock, et, errCh, tm := testStreamer(t, "TestStream_AlertFlapping", script)
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(13 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())

	// Flapping detection should drop the last alert.
	assert.Equal(5, requestCount)
}

func TestStream_InfluxDBOut(t *testing.T) {
	assert := assert.New(t)

	var script = `
stream
	.from("cpu")
	.where("host = 'serverA'")
	.window()
		.period(10s)
		.every(10s)
	.mapReduce(influxql.count("value"))
	.influxDBOut()
		.database("db")
		.retentionPolicy("rp")
		.measurement("m")
		.precision("s")
`
	done := make(chan error, 1)
	var points []imodels.Point
	var database string
	var rp string
	var precision string

	influxdb := NewMockInfluxDBService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//Respond
		var data client.Response
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(data)
		//Get request data
		database = r.URL.Query().Get("db")
		rp = r.URL.Query().Get("rp")
		precision = r.URL.Query().Get("precision")

		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			done <- err
			return
		}
		points, err = imodels.ParsePointsWithPrecision(b, time.Unix(0, 0), precision)
		done <- err
	}))

	clock, et, errCh, tm := testStreamer(t, "TestStream_InfluxDBOut", script)
	tm.InfluxDBService = influxdb
	defer tm.Close()

	// Move time forward
	clock.Set(clock.Zero().Add(15 * time.Second))
	// Wait till the replay has finished
	assert.Nil(<-errCh)
	// Wait till the task is finished
	assert.Nil(et.Err())
	// Wait till we received a request
	assert.Nil(<-done)

	assert.Equal("db", database)
	assert.Equal("rp", rp)
	assert.Equal("s", precision)
	if assert.Equal(1, len(points)) {
		p := points[0]
		assert.Equal("m", p.Name())
		assert.Equal(imodels.Fields(map[string]interface{}{"count": 10.0}), p.Fields())
		assert.Equal(imodels.Tags{}, p.Tags())
		t := time.Date(1970, 1, 1, 0, 0, 9, 0, time.UTC)
		assert.True(t.Equal(p.Time()), "times are not equal exp %s got %s", t, p.Time())
	}
}

// Helper test function for streamer
func testStreamer(t *testing.T, name, script string) (clock.Setter, *kapacitor.ExecutingTask, <-chan error, *kapacitor.TaskMaster) {
	assert := assert.New(t)
	if testing.Verbose() {
		wlog.LogLevel = wlog.DEBUG
	} else {
		wlog.LogLevel = wlog.OFF
	}

	//Create the task
	task, err := kapacitor.NewStreamer(name, script)
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Load test data
	dir, err := os.Getwd()
	if !assert.Nil(err) {
		t.FailNow()
	}
	data, err := os.Open(path.Join(dir, "data", name+".srpl"))
	if !assert.Nil(err) {
		t.FailNow()
	}
	c := clock.New(time.Unix(0, 0))
	r := kapacitor.NewReplay(c)

	// Create a new execution env
	tm := kapacitor.NewTaskMaster()
	tm.HTTPDService = httpService
	tm.Open()

	//Start the task
	et, err := tm.StartTask(task)
	if !assert.Nil(err) {
		t.FailNow()
	}

	// Replay test data to executor
	errCh := r.ReplayStream(data, tm.Stream)

	t.Log(string(et.Task.Dot()))
	return r.Setter, et, errCh, tm
}
