package main

import (
	"flag"
	"fmt"
	"github.com/appf-anu/chamber-tools"
	"github.com/mdaffin/go-telegraf"
	"github.com/ziutek/telnet"
	"log"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"encoding/xml"
	"net/http"
	"io/ioutil"
)

var (
	errLog *log.Logger
)

var (
	noMetrics, dummy, loopFirstDay            bool
	useLight1, useLight2, usehttp             bool
	address                                   string
	conditionsPath, hostTag, groupTag, didTag string
	interval                                  time.Duration
)

const DEBUG = false

const (
	matchFloatExp = `[-+]?\d*\.\d+|\d+`
	matchIntsExp  = `\b(\d+)\b`
)

// TsRegex is a regexp to find a timestamp within a filename
var /* const */ matchFloat = regexp.MustCompile(matchFloatExp)
var /* const */ matchInts = regexp.MustCompile(matchIntsExp)


const httpAuthString = "YWRtaW46ZmFkbWlu"

type PCOWeb struct {
	PCO PCO
}

type PCO struct {
	Analog []AnalogVar `xml:"ANALOG>VARIABLE"`
	Integer []IntegerVar `xml:"INTEGER>VARIABLE"`
}

type AnalogVar struct{
	//Index int `xml:"INDEX"`
	Value float64 `xml:"VALUE"`
}


type IntegerVar struct{
	//Index int `xml:"INDEX"`
	Value int `xml:"VALUE"`
}

var (
	// this is used because the convirons do not have an understanding of floating point numbers,
	// therefore 21.6c == 216 is used
	temperatureMultiplier = 10.0

	// these values are for controlling chambers
	temperatureDataIndex = 105
	humidityDataIndex    = 106
	light1DataIndex      = 107
	light2DataIndex      = 108
	//
	//// Conviron Control Sequences
	//// Give as a comma-seperated list of strings, each string consisting of
	////  "<Datatype> <Index> <Value>"
	//
	//// The init sequence is a sequence of strings passed to the set command which
	//// "setup" the conviron PCOweb controller to receive the temperature, humidity, and light settings.
	initCommand = "pcoset 0 I 100 26; pcoset 0 I 101 1; pcoset 0 I 102 1;"

	//// The teardown sequence happens at the end of each set of messages

	//// (not at the end of the connection)
	teardownCommand = "pcoset 0 I 121 1;"

	//// Command to clear the write flag, occurs after writing but before reloading.
	clearWriteFlagCommand = "pcoset 0 I 120 0;"

	//// Sequence to force reloading of the schedule, to make the written changes go live
	reloadSequence = "pcoset 0 I 100 7; pcoset 0 I 101 1; pcoset 0 I 102 1"
	//
	//// Command to clear the busy flag, occurs before exiting the connection
	clearBusyFlagCommand = "pcoset 0 I 123 0;"
	//// Command to set the busy flag
	setBusyFlagCommand             = "pcoset 0 I 123 1;"
	getChamberTimeCommand          = "pcoget 0 I 149 2;"
	secondaryGetChamberTimeCommand = "pcoget 0 I 45 2;"
)

// conviron indices start at 1
const (
	// it is extremely unlikely (see. impossible) that we will be measuring a humidity of 214,748,365 %RH or a
	// temperature of -340,282,346,638,528,859,811,704,183,484,516,925,440°C until we invent some new physics, so until
	// then, I will use these values as the unset or null values for HumidityTarget and TemperatureTarget
	nullTargetInt   = math.MinInt32
	nullTargetFloat = -math.MaxFloat32
)

// AValues type represent the temperature values for the chamber (I dont know why these are on a different row to
// everything else, but they are. They also all require dividing by 10.0 because they are returned as integers.)
type AValues struct {
	Temperature         float64 `idx:"1" multiplier:"10.0"`
	TemperatureTarget   float64 `multiplier:"10.0"`
	TemperatureSetPoint float64 `idx:"2" multiplier:"10.0"`
	CoilTemperature     float64 `idx:"3" multiplier:"10.0"`
}

// IValues type represents the other values that aren't temperature, like relative humidity and par
type IValues struct {
	Success                             string
	HeatCoolModulatingProportionalValve int `idx:"1"`
	RelativeHumidity                    int `idx:"4"`
	RelativeHumidityTarget              int
	RelativeHumiditySetPoint            int `idx:"5"`
	RelativeHumidityAdd                 int `idx:"6"`
	Par                                 int `idx:"11"`
	Light1Target                        int
	 Light1SetPoint                      int `idx:"12"`
	//Light1SetPoint                      int `idx:"23"`
	Light2Target int
	Light2SetPoint                      int `idx:"13"`
	//Light2SetPoint						int `idx:"24"`

	HiPressure   int `idx:"33"`
	LoPressure   int `idx:"34"`
	//IPAddressOctet1						int `idx:"47"`
	//IPAddressOctet2						int `idx:"48"`
	//IPAddressOctet3						int `idx:"49"`
	//IPAddressOctet4						int `idx:"50"`
}

type EnvironmentalStats struct {
	VapourPressureDeficit   float64
	SaturatedVapourPressure float64
	ActualVapourPressure    float64
	//MixingRatio float64
	//SaturatedMixingRatio float64
	AbsoluteHumidity float64 //(in kg/m³)
}


// DecodeValues decodes values in the array `values` and sets the values in the struct based on the `idx` tag,
// it also divides the values by the multiplier tag (which should be of the same type as the value).
func DecodeValues(values []int, i interface{}) error {

	v := reflect.ValueOf(i)

	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("decode requires non-nil pointer")
	}
	// get the value that the pointer v points to.
	v = v.Elem()
	// get type of v
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		ft := t.Field(i)
		// skip unexported fields. from godoc:
		// PkgPath is the package path that qualifies a lower case (unexported)
		// field name. It is empty for upper case (exported) field names.
		if ft.PkgPath != "" {
			continue
		}
		fv := v.Field(i)
		if idxString, ok := ft.Tag.Lookup("idx"); ok {
			if idx, err := strconv.ParseInt(idxString, 10, 64); err == nil {
				// the conviron idx starts at 1
				idx = idx - 1
				switch fv.Kind() {
				case reflect.Int:
					value := int64(values[idx])
					if multiplierString, ok := ft.Tag.Lookup("multiplier"); ok {
						if mult, err := strconv.ParseInt(multiplierString, 10, 64); err == nil {
							value /= mult
						}
					}

					fv.SetInt(value)
				case reflect.Float64, reflect.Float32:
					floatVal := float64(values[idx])
					if multiplierString, ok := ft.Tag.Lookup("multiplier"); ok {
						if mult, err := strconv.ParseFloat(multiplierString, 64); err == nil {
							floatVal /= mult
						}
					}

					fv.SetFloat(floatVal)
				case reflect.Bool:
					fv.SetBool(values[idx] != 0)
				}

			}

		}
	}
	return nil
}


var usage = func() {
	use := `
usage of %s:
flags:
	-no-metrics: don't send metrics to telegraf
	-dummy: don't control the chamber, only collect metrics (this is implied by not specifying a conditions file
	-conditions: conditions to use to run the chamber
	-interval: what interval to run conditions/record metrics at, set to 0s to read 1 metric and exit. (default=10m)

examples:
	collect data on 192.168.1.3  and output the errors to GC03-error.log and record the output to GC03.log
	%s -dummy 192.168.1.3 2>> GC03-error.log 1>> GC03.log

	run conditions on 192.168.1.3  and output the errors to GC03-error.log and record the output to GC03.log
	%s -conditions GC03-conditions.csv -dummy 192.168.1.3 2>> GC03-error.log 1>> GC03.log

quirks:
	the first 3 or 4 columns are used for running the chamber:
		date,time,temperature,humidity OR datetime,temperature,humidity
		the second case only occurs if the first 8 characters of the file (0th header) is "datetime"

	for the moment, the first line of the csv is technically (this is for your headers)
	if both -dummy and -no-metrics are specified, this program will exit.

`
	fmt.Printf(use, os.Args[0], os.Args[0], os.Args[0])
}

func chompAllValues(conn *telnet.Conn, command string) (values []int, err error) {

	// write command
	conn.Write([]byte(command + "\n"))
	// read 1 newline
	err = conn.SkipUntil("\n")
	if err != nil {
		return
	}

	// read another coz previous would be ours
	datad, err := conn.ReadString('#')

	if err != nil {
		return
	}
	// trim...
	data := strings.TrimSpace(string(datad))
	// find the ints
	tmpStrings := matchInts.FindAllString(data, -1)
	for _, v := range tmpStrings {
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return values, err
		}
		values = append(values, int(i))
	}
	return
}

func getValuesHttp(a *AValues, i *IValues) (err error) {

	redirectPolicyFunc := func(req *http.Request, via []*http.Request) error{
		req.SetBasicAuth("admin", "fadmin")
		return nil
	}
	client := &http.Client{
		CheckRedirect: redirectPolicyFunc,
	}
	req,_ := http.NewRequest("GET", "http://"+address+"/config/xml.cgi?I|1|35|A|1|3", nil)
	req.SetBasicAuth("admin", "fadmin")
	resp, err := client.Do(req)
	if err != nil{
		return
	}
	defer resp.Body.Close()
	container := PCOWeb{}
	if resp.StatusCode != http.StatusOK {
		errLog.Println("status code ", resp.StatusCode)
	}
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	err = xml.Unmarshal(bodyBytes, &container)
	if err != nil{
		return
	}
	var aValues []int
	for _,x := range container.PCO.Analog {
		aValues = append(aValues,int(x.Value*10.0)) // analog values are all multiply by 10
	}
	var iValues []int
	for _,x := range container.PCO.Integer {
		iValues = append(iValues,int(x.Value))
	}
	err = DecodeValues(aValues, a)
	if err != nil {
		return
	}
	err = DecodeValues(iValues, i)
	if err != nil {
		return
	}
	return
}

func getValues(a *AValues, i *IValues) (err error) {

	errLog.Println("Attempting telnet connection ...")
	conn, err := telnet.DialTimeout("tcp", address, time.Second*30)
	if err != nil {
		return
	}
	conn.SetUnixWriteMode(true)
	defer conn.Close()
	err = login(conn)
	if err != nil {
		return
	}

	aValues, err := chompAllValues(conn, "pcoget 0 A 1 3")
	if err != nil {
		return
	}
	iValues, err := chompAllValues(conn, "pcoget 0 I 1 35")
	if err != nil {
		return
	}
	err = DecodeValues(aValues, a)
	if err != nil {
		return
	}
	err = DecodeValues(iValues, i)
	if err != nil {
		return
	}
	return
}

func getEnvironmentalStats(temperature64, humidity64 float64, values *EnvironmentalStats) {
	// saturated vapor pressure
	values.SaturatedVapourPressure = 0.6108 * math.Exp(17.27*temperature64/(temperature64+237.3))
	// actual vapor pressure
	values.ActualVapourPressure = humidity64 / 100 * values.SaturatedVapourPressure
	// mixing ratio
	// values.mixingRatio = 621.97 * values.ActualVapourPressure / ((pressure64/10) - values.ActualVapourPressure)
	// saturated mixing ratio
	//values.SaturatedMixingRatio = 621.97 * values.SaturatedVapourPressure / ((pressure64/10) - values.SaturatedVapourPressure)
	// absolute humidity (in kg/m³)
	values.AbsoluteHumidity = (values.ActualVapourPressure / (461.5 * (temperature64 + 273.15))) * 1000

	// this equation returns a negative value (in kPa), which while technically correct,
	// is invalid in this case because we are talking about a deficit.
	values.VapourPressureDeficit = (values.ActualVapourPressure - values.SaturatedVapourPressure) * -1
}

func writeValues(a *AValues, i *IValues) (err error) {
	start := time.Now()
	defer func() {
		errLog.Println("time for last writeValues: ", time.Since(start))
	}()

	conn, err := telnet.DialTimeout("tcp", address, time.Second*30)
	if err != nil {
		return
	}
	conn.SetUnixWriteMode(true)
	defer conn.Close()
	err = login(conn)
	if err != nil {
		return
	}

	// if this results in no error, then we got the time in the chamber
	if t, err := chompAllValues(conn, getChamberTimeCommand); err == nil {
		if len(t) == 2 && t[0] == 0 && t[1] == 0 {
			// if they are both 0 then bingo, its midnight and the controller is busy reloading the program.
			errLog.Println("midnight in chamber, skipping...")
			return nil
		} else {
			if t2, err := chompAllValues(conn, secondaryGetChamberTimeCommand); err == nil {
				if len(t2) == 2 && t2[0] == 0 && t2[1] == 0 {
					return nil
				}
			}
		}
	} else {
		return nil
	}

	// make this happen from the struct
	tempCommand := fmt.Sprintf("pcoset 0 I %d %d; ", temperatureDataIndex, int(a.TemperatureTarget*temperatureMultiplier))
	humCommand := fmt.Sprintf("pcoset 0 I %d %d; ", humidityDataIndex, int(i.RelativeHumidityTarget))
	light1Command := fmt.Sprintf("pcoset 0 I %d %d; ", light1DataIndex, int(i.Light1Target))
	light2Command := fmt.Sprintf("pcoset 0 I %d %d; ", light2DataIndex, int(i.Light2Target))

	// set busy flag

	if _, err = chompAllValues(conn, setBusyFlagCommand); err != nil {
		return
	}
	time.Sleep(time.Millisecond * 100)
	if _, err = chompAllValues(conn, initCommand); err != nil {
		return
	}

	if _, err = chompAllValues(conn, tempCommand); err != nil {
		return
	}

	if _, err = chompAllValues(conn, humCommand); err != nil {
		return
	}

	if useLight1 {
		if _, err = chompAllValues(conn, light1Command); err != nil {
			return
		}
	}
	if useLight2 {
		if _, err = chompAllValues(conn, light2Command); err != nil {
			return
		}
	}

	if _, err = chompAllValues(conn, teardownCommand); err != nil {
		return
	}
	time.Sleep(time.Second * 2)
	if _, err = chompAllValues(conn, clearWriteFlagCommand); err != nil {
		return
	}
	if _, err = chompAllValues(conn, reloadSequence); err != nil {
		return
	}
	if _, err = chompAllValues(conn, teardownCommand); err != nil {
		return
	}
	time.Sleep(time.Second * 2)
	if _, err = chompAllValues(conn, clearWriteFlagCommand); err != nil {
		return
	}

	if _, err = chompAllValues(conn, clearBusyFlagCommand); err != nil {
		return
	}

	return
}

func login(conn *telnet.Conn) (err error) {
	err = conn.SkipUntil("login: ")
	if err != nil {
		return
	}

	conn.Write([]byte("root\n"))
	err = conn.SkipUntil("Password: ")
	if err != nil {
		return
	}

	conn.Write([]byte("froot\n"))
	err = conn.SkipUntil("# ")
	if err != nil {
		return
	}
	// END login
	return
}

// runStuff, should send values and write metrics.
// returns true if program should continue, false if program should retry
func runStuff(point *chamber_tools.TimePoint) bool {


	// round temperature to 1 decimal place
	a := AValues{TemperatureTarget: math.Round(point.Temperature*10) / 10}
	// round humidity to nearest integer
	i := IValues{RelativeHumidityTarget: int(math.Round(point.RelativeHumidity))}

	if useLight1 && chamber_tools.IndexConfig.Light1Idx > 0{

		i.Light1Target = point.Light1
	}
	if useLight2 && chamber_tools.IndexConfig.Light2Idx > 0{
		i.Light2Target = point.Light2
	}

	err := getValues(&a, &i)
	if err != nil {
		errLog.Println(err)
		time.Sleep(time.Second * 10)
		return false
	}
	errLog.Printf("%s (tgt|setp|act) t:(%.1f|%.1f|%.1f) rh:(%02d|%02d|%02d)",
		point.Datetime,
		a.TemperatureTarget,
		a.TemperatureSetPoint,
		a.Temperature,
		int(i.RelativeHumidityTarget),
		int(i.RelativeHumiditySetPoint),
		int(i.RelativeHumidity))
	if useLight1 || useLight2 {
		errLog.Printf("%s PAR: %d (tgt|setp) l1:(%01d|%01d) l2:(%01d|%01d)",
		point.Datetime,
		i.Par,
		int(i.Light1Target),
		int(i.Light1SetPoint),
		int(i.Light2Target),
		int(i.Light2SetPoint))
	}
	i.Success = "SUCCESS"
	if err = writeValues(&a, &i); err != nil {
		errLog.Println(err)
		i.Success = err.Error()
	}

	writeMetrics(a, i)
	return true
}

func decodeStructToMeasurement(m *telegraf.Measurement, va reflect.Value, i int) {
	f := va.Field(i)
	fi := f.Interface()
	n := va.Type().Field(i).Name

	switch v := fi.(type) {
	case int64:
		if v == nullTargetInt {
			break
		}
		m.AddInt64(n, v)
	case int32:
		m.AddInt32(n, v)
	case int:
		if v == nullTargetInt {
			break
		}
		m.AddInt(n, v)
	case float64:
		if v == nullTargetFloat {
			break
		}
		m.AddFloat64(n, v)
	case string:
		m.AddString(n, v)
	case bool:
		m.AddBool(n, v)
	}
}

func writeMetrics(av AValues, iv IValues) {

	if !noMetrics {
		telegrafHost := "telegraf:8092"
		if os.Getenv("TELEGRAF_HOST") != "" {
			telegrafHost = os.Getenv("TELEGRAF_HOST")
		}

		telegrafClient, err := telegraf.NewUDP(telegrafHost)
		if err != nil {
			errLog.Println(err)
			return
		}
		defer telegrafClient.Close()

		m := telegraf.NewMeasurement("conviron2")

		va := reflect.ValueOf(&av).Elem()
		for i := 0; i < va.NumField(); i++ {
			decodeStructToMeasurement(&m, va, i)
		}

		vi := reflect.ValueOf(&iv).Elem()
		for i := 0; i < vi.NumField(); i++ {
			decodeStructToMeasurement(&m, vi, i)
		}

		ev := EnvironmentalStats{}
		getEnvironmentalStats(av.Temperature, float64(iv.RelativeHumidity), &ev)
		ve := reflect.ValueOf(&ev).Elem()
		for i := 0; i < ve.NumField(); i++ {
			decodeStructToMeasurement(&m, ve, i)
		}

		if hostTag != "" {
			m.AddTag("host", hostTag)
		}
		if groupTag != "" {
			m.AddTag("group", groupTag)
		}
		if didTag != "" {
			m.AddTag("did", didTag)
		}

		telegrafClient.Write(m)
	}
}

func toInfluxLineProtocol(metricName string, valueStruct interface{}, t int64) string {
	s := reflect.ValueOf(valueStruct)

	// this will break things so just return emptystring
	if s.Kind() != reflect.Ptr || s.IsNil() {
		return ""
	}
	// get the value that the pointer v points to.
	s = s.Elem()
	typeOfT := s.Type()
	keyvaluepairs := make([]string, 0)
	keys := make([]string, 0)

	for i := 0; i < s.NumField(); i++ {
		keys = append(keys, typeOfT.Field(i).Name)
	}
	sort.Strings(keys)

	for _, key := range keys {
		f := s.FieldByName(key)
		val := f.Interface()

		switch v := val.(type) {
		case int:
			if v == nullTargetInt {
				break
			}
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=%di", key, v))
		case float64:
			if v == nullTargetFloat {
				break
			}
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=%f", key, v))
		case float32:
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=%f", key, v))
		case string:
			if v == "" {
				continue
			}
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=\"%s\"", key, v))
		case bool:
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=%t", key, v))
		}
	}
	csv := strings.Join(keyvaluepairs, ",")
	str := fmt.Sprintf("%s,host=%s,group=%s %s", metricName, hostTag, groupTag, csv)
	// add timestamp
	str = fmt.Sprintf("%s %d", str, t)
	return str
}

func init() {
	var err error
	hostname := os.Getenv("NAME")

	if address = os.Getenv("ADDRESS"); address == "" {
		address = flag.Arg(0)
		if err != nil {
			panic(err)
		}
	}

	errLog = log.New(os.Stderr, "[conviron] ", log.Ldate|log.Ltime|log.Lshortfile)

	// get the local zone and offset
	flag.Usage = usage
	flag.BoolVar(&noMetrics, "no-metrics", false, "dont collect metrics")
	if tempV := strings.ToLower(os.Getenv("NO_METRICS")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			noMetrics = true
		} else {
			noMetrics = false
		}
	}


	errLog.Println()

	flag.BoolVar(&dummy, "dummy", false, "dont send conditions to chamber")
	if tempV := strings.ToLower(os.Getenv("DUMMY")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			dummy = true
		} else {
			dummy = false
		}
	}


	flag.BoolVar(&usehttp, "use-http", false, "use http to get metrics instead of telnet")
	if tempV := strings.ToLower(os.Getenv("USE_HTTP")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			usehttp = true
		} else {
			usehttp = false
		}
	}

	flag.BoolVar(&useLight1, "use-light1", false, "use chamber internal light 1")
	if tempV := strings.ToLower(os.Getenv("USE_LIGHT1")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			useLight1 = true
		} else {
			useLight1 = false
		}
	}

	flag.BoolVar(&useLight2, "use-light2", false, "use chamber internal light 2")
	if tempV := strings.ToLower(os.Getenv("USE_LIGHT2")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			useLight2 = true
		} else {
			useLight2 = false
		}
	}

	flag.BoolVar(&loopFirstDay, "loop", false, "loop over the first day")
	if tempV := strings.ToLower(os.Getenv("LOOP")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			loopFirstDay = true
		} else {
			loopFirstDay = false
		}
	}

	flag.StringVar(&hostTag, "host-tag", hostname, "host tag to add to the measurements")
	if tempV := os.Getenv("HOST_TAG"); tempV != "" {
		hostTag = tempV
	}

	flag.StringVar(&groupTag, "group-tag", "nonspc", "host tag to add to the measurements")
	if tempV := os.Getenv("GROUP_TAG"); tempV != "" {
		groupTag = tempV
	}

	flag.StringVar(&didTag, "did-tag", "", "did specified tag")
	if tempV := os.Getenv("DID_TAG"); tempV != "" {
		didTag = tempV
	}

	flag.StringVar(&conditionsPath, "conditions", "", "conditions file to")
	if tempV := os.Getenv("CONDITIONS_FILE"); tempV != "" {
		conditionsPath = tempV
	}
	flag.DurationVar(&interval, "interval", time.Minute*10, "interval to run conditions/record metrics at")
	if tempV := os.Getenv("INTERVAL"); tempV != "" {
		interval, err = time.ParseDuration(tempV)
		if err != nil {
			errLog.Println("Couldnt parse interval from environment")
			errLog.Println(err)
		}
	}
	flag.Parse()

	if noMetrics && dummy {
		errLog.Println("dummy and no-metrics specified, nothing to do.")
		os.Exit(1)
	}
	if conditionsPath != "" && !dummy {
		chamber_tools.InitIndexConfig(errLog, conditionsPath)
		if chamber_tools.IndexConfig.TemperatureIdx == -1 || chamber_tools.IndexConfig.HumidityIdx == -1 {
			errLog.Println("No temperature or humidity headers found in conditions file" )
		}
	}
	errLog.Printf("noMetrics: \t%s\n", noMetrics)
	errLog.Printf("dummy: \t%s\n", dummy)
	errLog.Printf("loopFirstDay: \t%s\n", loopFirstDay)
	errLog.Printf("light1: \t%s\n", useLight1)
	errLog.Printf("light2: \t%s\n", useLight2)
	errLog.Printf("timezone: \t%s\n", chamber_tools.ZoneName)
	errLog.Printf("hostTag: \t%s\n", hostTag)
	errLog.Printf("groupTag: \t%s\n", groupTag)
	errLog.Printf("address: \t%s\n", address)
	errLog.Printf("file: \t%s\n", conditionsPath)
	errLog.Printf("interval: \t%s\n", interval)

}

func main() {

	if interval == time.Second*0 {
		a := AValues{TemperatureTarget: nullTargetFloat}
		i := IValues{RelativeHumidityTarget: nullTargetInt, Light1Target: nullTargetInt, Light2Target: nullTargetInt}
		var err error
		if usehttp {
			err = getValuesHttp(&a, &i)
		} else {
			err = getValues(&a, &i)
		}
		if err != nil {
			errLog.Println(err)
			os.Exit(1)
		}

		ev := EnvironmentalStats{}
		getEnvironmentalStats(a.Temperature, float64(i.RelativeHumidity), &ev)

		// print the line
		stra := toInfluxLineProtocol("conviron2", &a, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, stra)
		stri := toInfluxLineProtocol("conviron2", &i, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, stri)
		stre := toInfluxLineProtocol("conviron2", &ev, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, stre)
		os.Exit(0)
	}

	if !noMetrics && (conditionsPath == "" || dummy) {

		a := AValues{TemperatureTarget: nullTargetFloat}
		i := IValues{RelativeHumidityTarget: nullTargetInt}

		var err error
		if usehttp {
			err = getValuesHttp(&a, &i)
		} else {
			err = getValues(&a, &i)
		}

		if err != nil {
			errLog.Println(err)
		} else {
			// print the line
			stra := toInfluxLineProtocol("conviron2", &a, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, stra)
			stri := toInfluxLineProtocol("conviron2", &i, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, stri)
			ev := EnvironmentalStats{}
			getEnvironmentalStats(a.Temperature, float64(i.RelativeHumidity), &ev)
			stre := toInfluxLineProtocol("conviron2", &ev, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, stre)
			writeMetrics(a, i)
		}
		ticker := time.NewTicker(interval)
		go func() {
			for range ticker.C {
				a := AValues{TemperatureTarget: nullTargetFloat}
				i := IValues{RelativeHumidityTarget: nullTargetInt}

				var err error
				if usehttp {
					err = getValuesHttp(&a, &i)
				} else {
					err = getValues(&a, &i)
				}
				if err != nil {
					errLog.Println(err)
					continue
				}

				// print the line
				stra := toInfluxLineProtocol("conviron2", &a, time.Now().UnixNano())
				fmt.Fprintln(os.Stdout, stra)
				stri := toInfluxLineProtocol("conviron2", &i, time.Now().UnixNano())
				fmt.Fprintln(os.Stdout, stri)
				writeMetrics(a, i)
			}
		}()
		select {}
	}

	if conditionsPath != "" && !dummy {
		chamber_tools.RunConditions(errLog, runStuff, conditionsPath, loopFirstDay)
	}

}
