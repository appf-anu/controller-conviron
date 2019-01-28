package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/bcampbell/fuzzytime"
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
)

var (
	errLog     *log.Logger
	ctx        fuzzytime.Context
	zoneName   string
	zoneOffset int
)

var (
	noMetrics, dummy                  bool
	address                           string
	conditionsPath, hostTag, groupTag string
	interval                          time.Duration
)

const (
	matchFloatExp = `[-+]?\d*\.\d+|\d+`
	matchIntsExp  = `\b(\d+)\b`
)

// TsRegex is a regexp to find a timestamp within a filename
var /* const */ matchFloat = regexp.MustCompile(matchFloatExp)
var /* const */ matchInts = regexp.MustCompile(matchIntsExp)

var (
	// this is used because the convirons do not have an understanding of floating point numbers,
	// therefore 21.6c == 216 is used
	temperatureMultiplier = 10.0

	// these values are for controlling chambers, which is currently unimplemented
	//
	//// cant remember what these are used for
	temperatureDataIndex = 105
	humidityDataIndex    = 106
	//lightDataIndex       = 107
	//
	//// Conviron Control Sequences
	//// Give as a comma-seperated list of strings, each string consisting of
	////  "<Datatype> <Index> <Value>"
	//
	//// The init sequence is a sequence of strings passed to the set command which
	//// "setup" the conviron PCOweb controller to receive the temperature, humidity, and light settings.
	initCommand = []string{"pcoset 0 I 100 26\n", "pcoset 0 I 101 1\n", "pcoset 0 I 102 1\n"}

	//// The teardown sequence happens at the end of each set of messages
	//// (not at the end of the connection)
	teardownCommand = []string{"pcoset 0 I 123 1\n", "pcoset 0 I 121 1\n"}

	//// Command to clear the write flag, occurs after writing but before reloading.
	clearWriteFlagCommand = []string{"pcoset 0 I 120 0\n"}

	//// Sequence to force reloading of the schedule, to make the written changes go live
	reloadSequence = []string{"pcoset 0 I 100 7\n", "pcoset 0 I 101 1\n", "pcoset 0 I 102 1\n"}
	//
	//// Command to clear the busy flag, occurs before exiting the connection
	clearBusyFlagCommand = []string{"pcoset 0 I 123 0\n"}
)

const (
	// it is extremely unlikely (see. impossible) that we will be measuring a humidity of 214,748,365 %RH or a
	// temperature of -340,282,346,638,528,859,811,704,183,484,516,925,440°C until we invent some new physics, so until
	// then, I will use these values as the unset or null values for HumidityTarget and TemperatureTarget
	nullTargetInt   = math.MinInt32
	nullTargetFloat = -math.MaxFloat32
)

// conviron indices start at 1

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
	LightSetPoint                       int `idx:"12"`
	HiPressure                          int `idx:"33"`
	LoPressure                          int `idx:"34"`
	//IPAddressOctet1						int `idx:"47"`
	//IPAddressOctet2						int `idx:"48"`
	//IPAddressOctet3						int `idx:"49"`
	//IPAddressOctet4						int `idx:"50"`
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

func parseDateTime(tString string) (time.Time, error) {

	datetimeValue, _, err := ctx.Extract(tString)
	if err != nil {
		errLog.Printf("couldn't extract datetime: %s", err)
	}

	datetimeValue.Time.SetHour(datetimeValue.Time.Hour())
	datetimeValue.Time.SetMinute(datetimeValue.Time.Minute())
	datetimeValue.Time.SetSecond(datetimeValue.Time.Second())
	datetimeValue.Time.SetTZOffset(zoneOffset)

	return time.Parse("2006-01-02T15:04:05Z07:00", datetimeValue.ISOFormat())
}

func chompAllValues(conn *telnet.Conn, command string) (values []int, err error) {

	// write command
	conn.Write([]byte(command + "\n"))
	time.Sleep(time.Millisecond * 200)
	// read 1 newline
	err = conn.SkipUntil("\n")
	if err != nil {
		return
	}
	time.Sleep(time.Millisecond * 200)
	// read another coz previous would be ours
	datad, err := conn.ReadUntil("\n")
	if err != nil {
		return
	}
	// trim...
	data := strings.TrimSpace(string(datad))
	// find the ints
	tmpStrings := matchInts.FindAllString(data, -1)
	if len(tmpStrings) == 0 {
		err = fmt.Errorf("didnt get any values back from the chamber")
		return
	}
	for _, v := range tmpStrings {
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return values, err
		}
		values = append(values, int(i))
	}
	return
}

func login(conn *telnet.Conn) (err error) {
	time.Sleep(time.Second * 1)

	err = conn.SkipUntil("login: ")
	if err != nil {
		return
	}

	conn.Write([]byte("root\n"))
	time.Sleep(time.Millisecond * 200)

	err = conn.SkipUntil("Password: ")
	if err != nil {
		return
	}

	conn.Write([]byte("froot\n"))
	time.Sleep(time.Second * 1)

	err = conn.SkipUntil("# ")
	if err != nil {
		return
	}
	time.Sleep(time.Millisecond * 200)

	// END login
	return
}

func getValues(a *AValues, i *IValues) (err error) {
	conn, err := telnet.DialTimeout("tcp", address, time.Second*30)
	if err != nil {
		return
	}
	defer conn.Close()
	err = login(conn)
	if err != nil {
		return
	}

	aValues, err := chompAllValues(conn, "pcoget 0 A 1 3")
	if err != nil {
		return
	}
	iValues, err := chompAllValues(conn, "pcoget 0 I 1 64")
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

func writeValues(a *AValues, i *IValues) (err error) {

	conn, err := telnet.DialTimeout("tcp", address, time.Second*30)
	if err != nil {
		return
	}
	defer conn.Close()
	err = login(conn)
	if err != nil {
		return
	}

	var runSequence = func(seq []string) (err error) {
		for _, cmd := range seq {
			_, err = chompAllValues(conn, cmd)
			if err != nil {
				return
			}
		}
		time.Sleep(2)
		return nil
	}

	// make this happen from the struct
	tempCommand := fmt.Sprintf("pcoset 0 I %d %d\n", temperatureDataIndex, int(a.TemperatureTarget*temperatureMultiplier))
	humCommand := fmt.Sprintf("pcoset 0 I %d %d\n", humidityDataIndex, int(i.RelativeHumidityTarget))

	command_list := []string{}
	// concat initCommand to command list
	command_list = append(command_list, initCommand...)
	// run tempCommand and humcommand
	command_list = append(command_list, tempCommand, humCommand)
	// teardown
	command_list = append(command_list, teardownCommand...)
	if err = runSequence(command_list); err != nil {
		return
	}

	command_list = []string{}
	// clear write flag
	command_list = append(command_list, clearWriteFlagCommand...)
	if err = runSequence(command_list); err != nil {
		return
	}

	command_list = []string{}
	// reload
	command_list = append(command_list, reloadSequence...)
	// teardown again
	command_list = append(command_list, teardownCommand...)
	if err = runSequence(command_list); err != nil {
		return
	}

	command_list = []string{}
	// clear write flag again
	command_list = append(command_list, clearWriteFlagCommand...)
	// finally clear busy flag
	command_list = append(command_list, clearBusyFlagCommand...)
	if err = runSequence(command_list); err != nil {
		return
	}

	return
}


func runConditions() {
	errLog.Printf("running conditions file: %s\n", conditionsPath)
	file, err := os.Open(conditionsPath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	idx := 0
	var lastTime time.Time
	var lastLineSplit []string
	firstRun := true
	for scanner.Scan() {
		line := scanner.Text()
		if idx == 0 {
			idx++
			continue
		}

		lineSplit := strings.Split(line, ",")
		timeStr := lineSplit[0]
		theTime, err := parseDateTime(timeStr)
		if err != nil {
			errLog.Println(err)
			continue
		}

		// if we are before the time skip until we are after it
		// the -10s means that we shouldnt run again.
		if theTime.Before(time.Now()){
			lastLineSplit = lineSplit
			lastTime = theTime
			continue
		}

		if firstRun {
			firstRun = false
			errLog.Println("running firstrun line")
			for i:=0; i < 10; i++{
				if runStuff(lastTime, lastLineSplit) {
					break
				}
			}
		}

		errLog.Printf("sleeping for %ds\n",int(time.Until(theTime).Seconds()))
		time.Sleep(time.Until(theTime))

		// RUN STUFF HERE
		for i:=0; i < 10; i++{
			if runStuff(theTime, lineSplit) {
				break
			}
		}
		// end RUN STUFF
		idx++
	}
}


// runStuff, should send values and write metrics.
// returns true if program should continue, false if program should retry
func runStuff(theTime time.Time, lineSplit []string) bool {

	foundHum := matchFloat.FindString(lineSplit[3])
	if len(foundHum) < 0 {
		errLog.Println("no humidity value found")
		return true
	}

	humidity, err := strconv.ParseFloat(foundHum, 64)
	if err != nil {
		errLog.Println("failed parsing humidity float")
		return true
	}

	foundTemp := matchFloat.FindString(lineSplit[2])
	if len(foundTemp) < 0 {
		errLog.Println("no temperature value found")
		return true
	}

	temperature, err := strconv.ParseFloat(foundTemp, 64)
	if err != nil {
		errLog.Println("failed parsing temperature float")
		return true
	}

	// RUN STUFF HERE
	a := AValues{TemperatureTarget: temperature}
	i := IValues{RelativeHumidityTarget: int(math.Round(humidity))}

	err = getValues(&a, &i)
	if err != nil {
		errLog.Println(err)
		time.Sleep(time.Second * 10)
		return false
	}
	errLog.Printf("t %s \tt:\t%d\trh:%d \n", theTime, int(a.TemperatureTarget), int(i.RelativeHumidityTarget))
	errLog.Printf("c %s \tt:\t%d\trh:%d \n", theTime, int(a.Temperature), int(i.RelativeHumidity))
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
		if hostTag != "" {
			m.AddTag("host", hostTag)
		}
		if groupTag != "" {
			m.AddTag("group", groupTag)
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
	zoneName, zoneOffset = time.Now().Zone()

	ctx = fuzzytime.Context{
		DateResolver: fuzzytime.DMYResolver,
		TZResolver:   fuzzytime.DefaultTZResolver(zoneName),
	}
	flag.Usage = usage
	flag.BoolVar(&noMetrics, "no-metrics", false, "dont collect metrics")
	if tempV := strings.ToLower(os.Getenv("NO_METRICS")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			noMetrics = true
		} else {
			noMetrics = false
		}
	}

	flag.BoolVar(&dummy, "dummy", false, "dont send conditions to chamber")
	if tempV := strings.ToLower(os.Getenv("DUMMY")); tempV != "" {
		if tempV == "true" || tempV == "1" {
			dummy = true
		} else {
			dummy = false
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

	errLog.Printf("timezone: \t%s\n", zoneName)
	errLog.Printf("hostTag: \t%s\n", hostTag)
	errLog.Printf("groupTag: \t%s\n", groupTag)
	errLog.Printf("address: \t%s\n", address)
	errLog.Printf("file: \t%s\n", conditionsPath)
	errLog.Printf("interval: \t%s\n", interval)
}

func main() {

	if interval == time.Second*0 {
		a := AValues{TemperatureTarget: nullTargetFloat}
		i := IValues{RelativeHumidityTarget: nullTargetInt}
		err := getValues(&a, &i)
		if err != nil {
			errLog.Println(err)
			os.Exit(1)
		}
		// print the line
		stra := toInfluxLineProtocol("conviron2", &a, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, stra)
		stri := toInfluxLineProtocol("conviron2", &i, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, stri)
		os.Exit(0)
	}

	if !noMetrics && (conditionsPath == "" || dummy) {

		a := AValues{TemperatureTarget: nullTargetFloat}
		i := IValues{RelativeHumidityTarget: nullTargetInt}

		err := getValues(&a, &i)
		if err != nil {
			errLog.Println(err)
		} else {
			// print the line
			stra := toInfluxLineProtocol("conviron2", &a, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, stra)
			stri := toInfluxLineProtocol("conviron2", &i, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, stri)
			writeMetrics(a, i)
		}

		ticker := time.NewTicker(interval)
		go func() {
			for range ticker.C {
				a := AValues{TemperatureTarget: nullTargetFloat}
				i := IValues{RelativeHumidityTarget: nullTargetInt}

				err := getValues(&a, &i)
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
		runConditions()
	}

}
