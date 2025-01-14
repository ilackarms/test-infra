/*
Copyright 2019 The Knative Authors

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

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/knative/test-infra/shared/gcs"
	"github.com/knative/test-infra/shared/mysql"
	"github.com/knative/test-infra/tools/monitoring/alert"
	"github.com/knative/test-infra/tools/monitoring/config"
	"github.com/knative/test-infra/tools/monitoring/mail"
	msql "github.com/knative/test-infra/tools/monitoring/mysql"
	"github.com/knative/test-infra/tools/monitoring/prowapi"
	"github.com/knative/test-infra/tools/monitoring/subscriber"
)

var (
	dbConfig   *mysql.DBConfig
	mailConfig *mail.Config
	client     *subscriber.Client
	wfClient   *alert.Client
	db         *msql.DB

	alertEmailRecipients = []string{"knative-productivity-oncall@googlegroups.com"}
)

const (
	projectID = "knative-tests"
	subName   = "test-infra-monitoring-sub"
)

func main() {
	var err error

	dbName := flag.String("database-name", "monitoring", "The monitoring database name")
	dbPort := flag.String("database-port", "3306", "The monitoring database port")

	dbUserSF := flag.String("database-user", "/secrets/cloudsql/monitoringdb/username", "Database user secret file")
	dbPassSF := flag.String("database-password", "/secrets/cloudsql/monitoringdb/password", "Database password secret file")
	dbHost := flag.String("database-host", "/secrets/cloudsql/monitoringdb/host", "Database host secret file")
	mailAddrSF := flag.String("sender-email", "/secrets/sender-email/mail", "Alert sender email address file")
	mailPassSF := flag.String("sender-password", "/secrets/sender-email/password", "Alert sender email password file")

	serviceAccount := flag.String("service-account", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), "JSON key file for GCS service account")

	flag.Parse()

	dbConfig, err = mysql.ConfigureDB(*dbUserSF, *dbPassSF, *dbHost, *dbPort, *dbName)
	if err != nil {
		log.Fatal(err)
	}

	db, err = msql.NewDB(dbConfig)
	if err != nil {
		log.Fatal(err)
	}

	mailConfig, err = mail.NewMailConfig(*mailAddrSF, *mailPassSF)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	client, err = subscriber.NewSubscriberClient(ctx, projectID, subName)
	if err != nil {
		log.Fatalf("Failed to initialize the subscriber %+v", err)
	}

	err = gcs.Authenticate(context.Background(), *serviceAccount)
	if err != nil {
		log.Fatalf("Failed to authenticate gcs %+v", err)
	}

	wfClient = alert.Setup(client, db, &alert.MailConfig{Config: mailConfig, Recipients: alertEmailRecipients})

	// use PORT environment variable, or default to 8080
	port := "8080"
	if fromEnv := os.Getenv("PORT"); fromEnv != "" {
		port = fromEnv
	}

	// register hello function to handle all requests
	server := http.NewServeMux()
	server.HandleFunc("/hello", hello)
	server.HandleFunc("/test-conn", testCloudSQLConn)
	server.HandleFunc("/send-mail", sendTestEmail)
	server.HandleFunc("/test-sub", testSubscriber)
	server.HandleFunc("/test-insert", testInsert)
	server.HandleFunc("/start-alerting", testAlerting)

	// start the web server on port and accept requests
	log.Printf("Server listening on port %s", port)
	err = http.ListenAndServe(":"+port, server)
	log.Fatal(err)
}

// hello tests the as much completed steps in the entire monitoring workflow as possible
func hello(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	host, _ := os.Hostname()
	fmt.Fprintf(w, "Hello, world!\n")
	fmt.Fprintf(w, "Version: 1.0.0\n")
	fmt.Fprintf(w, "Hostname: %s\n", host)

	config, err := config.ParseDefaultConfig()
	if err != nil {
		log.Fatalf("Cannot parse yaml: %v", err)
	}

	errorPatterns := config.CollectErrorPatterns()
	fmt.Fprintf(w, "error patterns collected from yaml:%s", errorPatterns)
}

func testCloudSQLConn(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	fmt.Fprintf(w, "Testing mysql database connection...")

	_, err := dbConfig.Connect()
	if err != nil {
		fmt.Fprintf(w, "Failed to ping the database %v", err)
		return
	}
}

func sendTestEmail(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	log.Println("Sending test email")

	err := mailConfig.Send(
		alertEmailRecipients,
		"Test Subject",
		"Test Content",
	)
	if err != nil {
		fmt.Fprintf(w, "Failed to send email %v", err)
		return
	}

	fmt.Fprintln(w, "Sent the Email")
}

func testInsert(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	log.Println("testing insert to database")

	err := db.AddErrorLog("test error pattern", "test err message", "test job", 1, "gs://")
	if err != nil {
		fmt.Fprintf(w, "Failed to insert to database: %+v\n", err)
		return
	}

	fmt.Fprintln(w, "Success")
}

func testAlerting(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)

	wfClient.RunAlerting()
	log.Println("alerting workflow started")
}

func testSubscriber(w http.ResponseWriter, r *http.Request) {
	log.Printf("Serving request: %s", r.URL.Path)
	log.Println("Start listening to messages")

	go func() {
		err := client.ReceiveMessageAckAll(context.Background(), func(rmsg *prowapi.ReportMessage) {
			log.Printf("Report Message: %+v\n", rmsg)
		})
		if err != nil {
			log.Printf("Failed to retrieve messages due to %v", err)
		}
	}()
}
