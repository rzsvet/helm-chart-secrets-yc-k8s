package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"database/sql"

	env "github.com/Netflix/go-env"

	_ "github.com/lib/pq"

	"log"
	"net/http"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	"github.com/streadway/amqp"
	// "helm-secrets/internal/requests"
)

var (
	db    *sql.DB
	queue amqp.Queue
	ch    *amqp.Channel
)

type environment struct {
	PgsqlURI      string `env:"PGSQL_URI"`
	Listen        string `env:"LISTEN"`
	RabbitURI     string `env:"RABBIT_URI"`
	MigrationPath string `env:"MIGRATION_PATH"`
	ApiToken      string `env:"API_TOKEN"`
}

type jsonResponse struct {
	Success bool         `json:"success"`
	Message string       `json:"message"`
	Data    *interface{} `json:"data"`
}

type HealthStatus struct {
	Status    string            `json:"status"`
	Timestamp time.Time         `json:"timestamp"`
	Checks    map[string]string `json:"checks"`
}

func returnResponse(code int, msg string, data *interface{}, w http.ResponseWriter) {
	success := true
	if code >= 400 {
		success = false
	}
	respStruct := &jsonResponse{Success: success, Message: msg, Data: data}

	resp, _ := json.Marshal(respStruct)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(resp)
	w.Write([]byte("\n"))

	return
}

func main() {
	var err error

	// Parse command line flags
	healthCheck := flag.Bool("health-check", false, "Run health check and exit")
	flag.Parse()

	// Getting configuration
	log.Printf("INFO: Getting environment variables\n")
	cnf := environment{}
	_, err = env.UnmarshalFromEnviron(&cnf)
	if err != nil {
		log.Fatal(err)
	}

	// If health check flag is set or environment variable is true, run health check and exit
	if *healthCheck {
		log.Printf("INFO: Check services statuses\n")
		status := runHealthCheck(cnf)
		printHealthStatus(status)
		if status.Status == "healthy" {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	// Connecting to database
	log.Printf("INFO: Connecting to database")
	db, err = sql.Open("postgres", cnf.PgsqlURI)
	if err != nil {
		log.Fatalf("Can't connect to postgresql: %v", err)
	}
	defer db.Close()

	// Test database connection
	if err := db.Ping(); err != nil {
		log.Fatalf("Can't ping database: %v", err)
	}

	// Running migrations
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		log.Fatalf("Can't get postgres driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+cnf.MigrationPath, "postgres", driver)
	if err != nil {
		log.Fatalf("Can't get migration object: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Fatalf("Migration failed: %v", err)
	}

	// Initialising rabbit mq
	conn, err := amqp.Dial(cnf.RabbitURI)
	if err != nil {
		log.Fatalf("Can't connect to rabbitmq: %v", err)
	}
	defer conn.Close()

	ch, err = conn.Channel()
	if err != nil {
		log.Fatalf("Can't open channel: %v", err)
	}
	defer ch.Close()

	err = initRabbit()
	if err != nil {
		log.Fatalf("Can't create rabbitmq queues: %s\n", err)
	}

	// Setting handlers for query
	router := mux.NewRouter().StrictSlash(true)

	// Health check endpoint
	router.HandleFunc("/health", healthCheckHandler).Methods("GET")

	// PROJECTS
	router.HandleFunc("/requests", authMiddleware(getRequests, cnf)).Methods("GET")
	router.HandleFunc("/requests", authMiddleware(addRequest, cnf)).Methods("POST")
	router.HandleFunc("/requests/{name}", authMiddleware(getRequest, cnf)).Methods("GET")
	router.HandleFunc("/requests/{name}", authMiddleware(updRequest, cnf)).Methods("PUT")
	router.HandleFunc("/requests/{name}", authMiddleware(delRequest, cnf)).Methods("DELETE")

	address := fmt.Sprintf(":%s", cnf.Listen)
	log.Printf("INFO: Starting listening on %s\n", address)

	s := &http.Server{
		Addr:         address,
		Handler:      handlers.LoggingHandler(os.Stderr, router),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGTERM)

	go func() {
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	select {
	case <-signals:
		log.Printf("server recieve shutdown signal...")
		// Shutdown the server when the context is canceled
		s.Shutdown(ctx)
	}
}

// Health check handler for HTTP endpoint
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	var cnf environment
	_, err := env.UnmarshalFromEnviron(&cnf)
	if err != nil {
		http.Error(w, "Configuration error", http.StatusInternalServerError)
		return
	}

	status := runHealthCheck(cnf)

	w.Header().Set("Content-Type", "application/json")
	if status.Status == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(status)
}

// Run comprehensive health check
func runHealthCheck(cnf environment) HealthStatus {
	status := HealthStatus{
		Status:    "healthy",
		Timestamp: time.Now(),
		Checks:    make(map[string]string),
	}

	// Check database connection
	if err := checkDatabase(cnf.PgsqlURI); err != nil {
		status.Status = "unhealthy"
		status.Checks["database"] = fmt.Sprintf("ERROR: %v", err)
	} else {
		status.Checks["database"] = "OK"
	}

	// Check RabbitMQ connection
	if err := checkRabbitMQ(cnf.RabbitURI); err != nil {
		status.Status = "unhealthy"
		status.Checks["rabbitmq"] = fmt.Sprintf("ERROR: %v", err)
	} else {
		status.Checks["rabbitmq"] = "OK"
	}

	// Check if migration path exists
	if _, err := os.Stat(cnf.MigrationPath); os.IsNotExist(err) {
		status.Status = "unhealthy"
		status.Checks["migrations"] = fmt.Sprintf("ERROR: Migration path not found: %s", cnf.MigrationPath)
	} else {
		status.Checks["migrations"] = "OK"
	}

	return status
}

func printHealthStatus(status HealthStatus) {
	fmt.Println("🔍 Application Health Check")
	fmt.Println("───────────────────────────")
	fmt.Printf("Status:    %s\n", status.Status)
	fmt.Printf("Timestamp: %s\n", status.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Println("───────────────────────────")
	fmt.Println("Checks:")

	for key, value := range status.Checks {
		icon := "✅"
		if value != "OK" {
			icon = "❌"
		}
		fmt.Printf("  %s %-12s: %s\n", icon, key, value)
	}
	fmt.Println("───────────────────────────")

	if status.Status == "healthy" {
		fmt.Println("🎉 All systems operational!")
	} else {
		fmt.Println("💥 Some systems need attention!")
	}
}

// Check database connectivity
func checkDatabase(connectionString string) error {
	db, err := sql.Open("postgres", connectionString)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping failed: %v", err)
	}

	// Test simple query
	var result int
	err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
	if err != nil {
		return fmt.Errorf("query test failed: %v", err)
	}

	if result != 1 {
		return fmt.Errorf("unexpected query result: %d", result)
	}

	return nil
}

// Check RabbitMQ connectivity
func checkRabbitMQ(connectionString string) error {
	conn, err := amqp.Dial(connectionString)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: %v", err)
	}
	defer ch.Close()

	return nil
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Not Implemented\n"))
}

func authMiddleware(next http.HandlerFunc, cnf environment) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString := r.Header.Get("X-API-KEY")
		if tokenString != cnf.ApiToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Missing Authorization Header\n"))
			return
		}

		next(w, r)
	})
}

func initRabbit() error {
	err := ch.ExchangeDeclare(
		"VideoParserExchange", // name
		"fanout",              // type
		true,                  // durable
		false,                 // auto delete
		false,                 // internal
		false,                 // no wait
		nil,                   // arguments
	)
	if err != nil {
		return err
	}

	err = ch.ExchangeDeclare(
		"VideoParserRetryExchange", // name
		"fanout",                   // type
		true,                       // durable
		false,                      // auto delete
		false,                      // internal
		false,                      // no wait
		nil,                        // arguments
	)
	if err != nil {
		return err
	}

	args := amqp.Table{"x-dead-letter-exchange": "VideoParserRetryExchange"}

	queue, err = ch.QueueDeclare(
		"VideoParserWorkerQueue", // name
		true,                     // durable - flush to disk
		false,                    // delete when unused
		false,                    // exclusive - only accessible by the connection that declares
		false,                    // no-wait - the queue will assume to be declared on the server
		args,                     // arguments -
	)
	if err != nil {
		return err
	}

	args = amqp.Table{"x-dead-letter-exchange": "VideoParserExchange", "x-message-ttl": 60000}
	queue, err = ch.QueueDeclare(
		"VideoParserWorkerRetryQueue", // name
		true,                          // durable - flush to disk
		false,                         // delete when unused
		false,                         // exclusive - only accessible by the connection that declares
		false,                         // no-wait - the queue will assume to be declared on the server
		args,                          // arguments -
	)
	if err != nil {
		return err
	}

	queue, err = ch.QueueDeclare(
		"VideoParserArchiveQueue", // name
		true,                      // durable - flush to disk
		false,                     // delete when unused
		false,                     // exclusive - only accessible by the connection that declares
		false,                     // no-wait - the queue will assume to be declared on the server
		nil,                       // arguments -
	)
	if err != nil {
		return err
	}

	err = ch.QueueBind("VideoParserWorkerQueue", "*", "VideoParserExchange", false, nil)
	if err != nil {
		return err
	}

	err = ch.QueueBind("VideoParserArchiveQueue", "*", "VideoParserExchange", false, nil)
	if err != nil {
		return err
	}

	err = ch.QueueBind("VideoParserWorkerRetryQueue", "*", "VideoParserRetryExchange", false, nil)
	if err != nil {
		return err
	}

	return nil
}
