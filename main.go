package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/bcampbell/fuzzytime"
	"github.com/mdaffin/go-telegraf"
	"github.com/ziutek/telnet"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	errLog         *log.Logger
	ctx            fuzzytime.Context
	zoneName       string
	zoneOffset     int
	telegrafClient telegraf.Client
	telegrafErr    error
)

var (
	noMetrics, dummy        bool
	conditionsPath, hostTag string
	interval                time.Duration
)

const (
	matchFloatExp = `[-+]?\d*\.\d+|\d+`
	matchIntsExp  = `\b(\d+)\b`
)

// TsRegex is a regexp to find a timestamp within a filename
var /* const */ matchFloat = regexp.MustCompile(matchFloatExp)
var /* const */ match2Ints = regexp.MustCompile(matchIntsExp)

var (
	// this is used because the convirons do not have an understanding of floating point numbers,
	// therefore 21.6c == 216 is used
	temperatureMultiplier = 10.0

	// these values are for controlling chambers, which is currently unimplemented
	//
	//// cant remember what these are used for
	//temperatureDataIndex = 105
	//humidityDataIndex    = 106
	//lightDataIndex       = 107
	//
	//// Conviron Control Sequences
	//// Give as a comma-seperated list of strings, each string consisting of
	////  "<Datatype> <Index> <Value>"
	//
	//// The init sequence is a sequence of strings passed to the set command which
	//// "setup" the conviron PCOweb controller to receive the temperature, humidity, and light settings.
	//initCommand = []string{"I 100 26", "I 101 1", "I 102 1"}
	//
	//// The teardown sequence happens at the end of each set of messages
	//// (not at the end of the connection)
	//teardownCommand = []string{"I 123 1", "I 121 1"}
	//
	//// Command to clear the write flag, occurs after writing but before reloading.
	//clearWriteFlagCommand = []string{"I 120 0"}
	//
	//// Sequence to force reloading of the schedule, to make the written changes go live
	//reloadSequence = []string{"I 100 7", "I 101 1", "I 102 1"}
	//
	//// Command to clear the busy flag, occurs before exiting the connection
	//clearBusyFlagCommand = []string{"I 123 0"}
)

const (
	tempCommand = "pcoget 0 A 1 2"
	rhCommand   = "pcoget 0 I 4 2"
	parCommand  = "pcoget 0 I 11 1"
)

var usage = func() {
	use := `
usage of %s:
flags:
	-no-metrics: dont collect or send metrics to telegraf
	-dummy: dont control the chamber, only collect metrics
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
	fmt.Printf(use, os.Args[0])
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

func getValue(conn *telnet.Conn, command string, multiplier float64) (valueRecorded, valueSet float64, err error) {
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
	tmpStrings := match2Ints.FindAllString(data, 2)
	if len(tmpStrings) == 0 {
		err = fmt.Errorf("didnt get any values back from the chamber")
		return
	}

	valueRecordedInt, err := strconv.Atoi(tmpStrings[0])
	if err != nil {
		return
	}
	valueRecorded = float64(valueRecordedInt) / multiplier
	if len(tmpStrings) < 2 {
		valueSet = valueRecorded
		return
	}
	valueSetInt, err := strconv.Atoi(tmpStrings[1])
	if err != nil {
		return
	}
	valueSet = float64(valueSetInt) / multiplier
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

func getValues(ip string) (values map[string]interface{}, err error) {
	values = make(map[string]interface{})
	conn, err := telnet.DialTimeout("tcp", ip, time.Second*30)
	if err != nil {
		return
	}
	defer conn.Close()
	err = login(conn)
	if err != nil {
		return
	}
	tempRecorded, tempSet, err := getValue(conn, tempCommand, temperatureMultiplier)
	values["temp_recorded"] = tempRecorded
	values["temp_set"] = tempSet

	// END login
	humRecorded, humSet, err := getValue(conn, rhCommand, 1)
	values["humidity_recorded"] = humRecorded
	values["humidity_set"] = humSet

	// END login
	parRecorded, _, err := getValue(conn, parCommand, 1)
	values["par_recorded"] = parRecorded
	return
}

func runConditions() {
	file, err := os.Open(conditionsPath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	idx := 0
	oldIndices := true

	for scanner.Scan() {
		line := scanner.Text()
		if idx == 0 {
			if line[:8] == "datetime" {
				oldIndices = false
			}
			idx++
			continue
		}

		lineSplit := strings.Split(line, ",")
		var theTime time.Time

		var timeStr, humidityStr, temperatureStr string
		var temperature, humidity float64

		if oldIndices {
			timeStr = lineSplit[0] + " " + lineSplit[1]
			temperatureStr = lineSplit[2]
			humidityStr = lineSplit[3]

		} else {
			timeStr = lineSplit[0]
			temperatureStr = lineSplit[1]
			humidityStr = lineSplit[2]
		}
		theTime, err := parseDateTime(timeStr)
		if err != nil {
			errLog.Println(err)
			continue
		}
		// if we are before the time skip until we are after it
		if theTime.Before(time.Now()) {
			continue
		}

		foundHum := matchFloat.FindString(humidityStr)
		if len(foundHum) < 0 {
			errLog.Println("no humidity value found")
			continue
		}

		humidity, err = strconv.ParseFloat(foundHum, 64)
		if err != nil {
			errLog.Println("failed parsing humidity float")
			continue
		}

		foundTemp := matchFloat.FindString(temperatureStr)
		if len(foundTemp) < 0 {
			errLog.Println("no temperature value found")
			continue
		}

		temperature, err = strconv.ParseFloat(foundTemp, 64)
		if err != nil {
			errLog.Println("failed parsing temperature float")
			continue
		}
		temperature *= temperatureMultiplier

		// RUN STUFF HERE
		fmt.Println(theTime, temperature, humidity)

		if !noMetrics {
			if telegrafErr != nil {
				m := telegraf.NewMeasurement("conviron")
				m.AddFloat64("temp_target", temperature/temperatureMultiplier)
				m.AddFloat64("humidity_target", humidity)
				m.AddTag("host", hostTag)
				telegrafClient.Write(m)
			}

		}

		// end RUN STUFF

		idx++
		errLog.Printf("sleeping for %ds\n", int(time.Until(theTime).Seconds()))
		time.Sleep(time.Until(theTime))
	}

}

func toInfluxLineProtocol(metricName string, values map[string]interface{}, t int64) string {

	keyvaluepairs := make([]string, 0)

	keys := make([]string, 0)
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		val := values[key]
		switch v := val.(type) {
		case int:
			keyvaluepairs = append(keyvaluepairs, fmt.Sprintf("%s=%di", key, v))
		case float64:
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
	str := fmt.Sprintf("%s,host=%s %s", metricName, hostTag, csv)
	// add timestamp
	str = fmt.Sprintf("%s %d", str, t)
	return str
}


func init() {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	errLog = log.New(os.Stderr, "[conviron] ", log.Ldate|log.Ltime|log.Lshortfile)
	// get the local zone and offset
	zoneName, zoneOffset = time.Now().Zone()
	errLog.Printf("timezone: %s\n", zoneName)

	ctx = fuzzytime.Context{
		DateResolver: fuzzytime.DMYResolver,
		TZResolver:   fuzzytime.DefaultTZResolver(zoneName),
	}
	flag.Usage = usage
	flag.BoolVar(&noMetrics, "no-metrics", false, "dont collect metrics")
	flag.BoolVar(&dummy, "dummy", false, "dont send conditions to chamber")
	flag.StringVar(&hostTag, "host-tag", hostname, "host tag to add to the measurements")
	flag.StringVar(&conditionsPath, "conditions", "", "conditions file to")
	flag.DurationVar(&interval, "interval", time.Minute*10, "interval to run conditions/record metrics at")
	flag.Parse()
	if noMetrics && dummy {
		errLog.Println("dummy and no-metrics specified, nothing to do.")
		os.Exit(1)
	}
}

func main() {
	if !noMetrics {
		telegrafClient, telegrafErr = telegraf.NewUnix("/tmp/telegraf.sock")
		if telegrafErr != nil {
			errLog.Println(telegrafErr)
		}
		defer telegrafClient.Close()

	}
	errLog.Println("getting values from "+flag.Arg(0))
	if interval == time.Second*0 {
		values, err := getValues(flag.Arg(0))
		if err != nil {
			errLog.Println(err)
			os.Exit(1)
		}
		//
		str := toInfluxLineProtocol("conviron", values, time.Now().UnixNano())
		fmt.Fprintln(os.Stdout, str)
		os.Exit(0)
	}

	if !noMetrics && conditionsPath == "" {
		ticker := time.NewTicker(interval)

		for range ticker.C{
			values, err := getValues(flag.Arg(0))
			if err != nil {
				errLog.Println(err)
			}
			// print the line
			str := toInfluxLineProtocol("conviron", values, time.Now().UnixNano())
			fmt.Fprintln(os.Stdout, str)
			if telegrafErr == nil {
				m := telegraf.NewMeasurement("conviron")
				for k, v := range values {
					m.AddFloat64(k, v.(float64))
				}
				m.AddTag("host", hostTag)
				telegrafClient.Write(m)
			}
		}

	}

	if conditionsPath != "" {
		runConditions()
	}

}
