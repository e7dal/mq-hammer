// Copyright 2019 Koninklijke KPN N.V.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	mqtt "github.com/rollulus/paho.mqtt.golang"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	_ "net/http/pprof"

	"net/http"
)

var brokerAddr string
var scenarioFile string
var referenceFile string
var nAgents int
var sleep time.Duration
var clientIDPrefix string
var agentLogFormat string
var credentialsFile string
var insecure bool
var disableMqttTLS bool
var prometheusSrv string
var nssKeyLogFile string
var verbose bool
var verboser bool
var username string
var password string

func init() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	fs := rootCmd.PersistentFlags()
	fs.StringVarP(&brokerAddr, "broker", "b", "", "Broker address, host[:port]; port defaults to 8883 for TLS or 1883 for plain text ")
	fs.StringVarP(&scenarioFile, "scenario", "s", "", "Filename containing the scenario as JSON")
	fs.StringVarP(&referenceFile, "ref", "r", "", "Filename with the expected reference data as JSON")
	fs.IntVarP(&nAgents, "num-agents", "n", 1, "Number of agents to spin up")
	fs.DurationVar(&sleep, "sleep", 250*time.Millisecond, "Duration to wait between spinning up each agent")
	fs.StringVar(&clientIDPrefix, "client-id", "mq-hammer:"+GetVersion().GitTag+":", "Client ID prefix; a UUID is appended to it per agent to guarantee uniqueness")
	fs.StringVar(&agentLogFormat, "agent-logs", "", "Filename to output per-agent logs. Go-templated, e.g. 'agent-{{ .ClientID }}.log', or - to log to stderr")
	fs.StringVarP(&username, "username", "u", "", "Username for connecting")
	fs.StringVarP(&password, "password", "p", "", "Password for connecting")
	fs.StringVar(&credentialsFile, "credentials", "", "Filename with username,password and client id in CSV")
	fs.BoolVarP(&insecure, "insecure", "k", false, "Don't validate TLS hostnames / cert chains")
	fs.BoolVar(&disableMqttTLS, "disable-mqtt-tls", false, "Disable TLS for MQTT, use plain tcp sockets to the MQTT broker")
	fs.StringVar(&nssKeyLogFile, "nss-key-log", "", "Filename to append TLS master secrets in NSS key log format to")
	fs.StringVar(&prometheusSrv, "prometheus", ":8080", "Export Prometheus metrics at this address")
	fs.BoolVarP(&verbose, "verbose", "v", false, "Verbose: output paho mqtt's internal logging (crit, err and warn) to stderr")
	fs.BoolVarP(&verboser, "verboser", "w", false, "Verboser: output paho mqtt's internal logging (crit, err, warn and debug) to stderr")

	rootCmd.AddCommand(versionCmd)

	if err := viper.BindPFlags(fs); err != nil {
		panic(err)
	}
	viper.AutomaticEnv()
}

var rootCmd = &cobra.Command{
	Use:   "mqhammer",
	Short: "MQ Hammer is an MQTT load testing tool",
	Long: `MQ Hammer is an MQTT load testing tool

    MQ Hammer will create --num-agents goroutines, each subscribing, unsubscribing
    and publish to topics at given timestamps according to the instructions in the
    --scenario file.

    It is possible to provide a reference data set that can be used for validation
    of static (and retained) data. With it, MQ Hammer knows when after a
    subscription the complete contents came in, how long it took to complete, if
    messages are missing, etc.

    By default, agents do not log to stderr to keep things clean a bit. For
    diagnostics, it is possible through --agent-logs to output logs to a file, one
    for each agent. Giving --agent-logs=- as argument will make the agents log to the
    default logger.

    All arguments can be specified through identically named environment variables
    as well.`,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if brokerAddr == "" {
			return errors.New("broker cannot be empty")
		}

		// plug in default ports if unspecified
		if !strings.Contains(brokerAddr, ":") {
			if disableMqttTLS {
				brokerAddr += ":1883"
			} else {
				brokerAddr += ":8883"
			}
		}

		// not sure if this is due diligence or plain silly, but it might be too tempting to crank up the number for someone following the quick start
		if strings.Contains(brokerAddr, "iot.eclipse.org") && nAgents > 4 {
			nAgents = 4
		}

		if scenarioFile == "" {
			return errors.New("scenario cannot be empty")
		}

		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		ver := GetVersion()
		logrus.WithFields(logrus.Fields{"GitCommit": ver.GitCommit, "GitTag": ver.GitTag, "SemVer": ver.SemVer}).Infof("this is MQ Hammer")

		// verbose?
		if verbose || verboser {
			l := logrus.StandardLogger()
			mqtt.CRITICAL = l
			mqtt.ERROR = l
			mqtt.WARN = l
		}
		if verboser {
			mqtt.DEBUG = logrus.StandardLogger()
		}

		// load scenario
		logrus.WithFields(logrus.Fields{"scenario": scenarioFile}).Infof("load scenario")
		scenario, err := newScenarioFromFile(scenarioFile)
		if err != nil {
			return err
		}

		// load optional reference data set
		var refSet ReferenceSet
		if referenceFile != "" {
			logrus.WithFields(logrus.Fields{"referenceFile": referenceFile}).Infof("load reference data")
			refSet, err = newReferenceSetFromFile(referenceFile)
			if err != nil {
				return err
			}
		}

		// mqtt creds from file
		var creds MqttCredentials = &fixedCreds{clientID: clientIDPrefix, username: username, password: password}
		if credentialsFile != "" { // creds from file
			logrus.WithFields(logrus.Fields{"credentialsFile": credentialsFile}).Infof("load credentials from file")
			fcreds, err := newMqttCredentialsFromFile(credentialsFile)
			if err != nil {
				return err
			}
			if nAgents > fcreds.Size() { // this test is only approximate, tokens might be used for mqtt metrics and distributed mode as well
				return fmt.Errorf("cannot create %d agents with only %d tokens provided", nAgents, fcreds.Size())
			}
			creds = fcreds
			logrus.WithFields(logrus.Fields{"credentialsFile": credentialsFile, "nCredentials": fcreds.Size()}).Infof("loaded credentials from file")
		}

		// tls config shared between agents
		tlsCfg := &tls.Config{InsecureSkipVerify: insecure}
		if nssKeyLogFile != "" {
			w, err := os.OpenFile(nssKeyLogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				return err
			}
			logrus.WithFields(logrus.Fields{"filename": nssKeyLogFile}).Info("write TLS master secrets in NSS key log format")
			tlsCfg.KeyLogWriter = w
		}

		metricsHandlers := []MetricsHandler{&consoleMetrics{}}

		// expose prometheus metrics?
		if prometheusSrv != "" {
			logrus.WithFields(logrus.Fields{"address": prometheusSrv}).Info("export Prometheus metrics")
			pm, err := newPrometheusMetrics(prometheusSrv)
			if err != nil {
				return err
			}
			metricsHandlers = append(metricsHandlers, pm)
		}

		// start event funnel processing
		eventFunnel := newChanneledFunnel(metricsHandlers...)
		go eventFunnel.Process()

		// create agent controller
		ctl := newAgentController(brokerAddr, tlsCfg, nAgents, agentLogFormat, sleep, creds, eventFunnel, refSet, scenario)
		if disableMqttTLS {
			ctl.disableMqttTLS = true
		}

		// run!
		logrus.Infof("go")
		ctl.Control()

		return nil
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of MQ Hammer",
	Run: func(cmd *cobra.Command, args []string) {
		ver := GetVersion()
		fmt.Printf("MQ Hammer %s (%s) %s\n", ver.SemVer, ver.GitTag, ver.GitCommit)
	}}

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
