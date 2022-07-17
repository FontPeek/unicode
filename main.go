package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/cors"
	_ "github.com/jackc/pgx/v4"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "github.com/lib/pq"

	_ "github.com/fontpeek/unicode/docs"
	"github.com/go-chi/chi/v5"
	"github.com/riandyrn/otelchi"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/grpc/credentials"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var pgPool *sqlx.DB
var err error

var (
	serviceName  = os.Getenv("SERVICE_NAME")
	collectorURL = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	insecure     = os.Getenv("INSECURE_MODE")
)

func initTracerProvider() *sdktrace.TracerProvider {

	secureOption := otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, ""))
	if len(insecure) > 0 {
		secureOption = otlptracegrpc.WithInsecure()
	}

	exporter, err := otlptrace.New(
		context.Background(),
		otlptracegrpc.NewClient(
			secureOption,
			otlptracegrpc.WithEndpoint(collectorURL),
		),
	)
	if err != nil {
		log.Fatal(err)
	}
	res, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			// the service name used to display traces in backends
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		log.Fatalf("unable to initialize resource due: %v", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
}

func intersection(s1, s2 []string) (inter []string) {
	hash := make(map[string]bool)
	for _, e := range s1 {
		hash[e] = true
	}
	for _, e := range s2 {
		if hash[e] {
			inter = append(inter, e)
		}
	}
	return
}

// @title FontPeek Public Unicode API
// @version 1.0
// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html
// @syntaxHighlight.activate false
// @host https://unicode.fontpeek.com
// @BasePath /
func main() {

	for {
		adminConnStr := fmt.Sprintf("postgres://%s:%s@%s:5432/unicode?sslmode=disable", os.Getenv("DB_USERNAME"), os.Getenv("DB_PASS"), "localhost")
		pgPool, err = sqlx.Open("postgres", adminConnStr)
		if err == nil {
			break
		}
		fmt.Fprintf(os.Stderr, "Unable to connect to database: %v\n", err)
		time.Sleep(time.Second * 5)
	}
	defer pgPool.Close()
	pgPool.SetMaxOpenConns(15)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	// initialize trace provider
	tp := initTracerProvider()
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
	}()
	// set global tracer provider & text propagators
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// define router
	r := chi.NewRouter()
	r.Use(otelchi.Middleware("unicode", otelchi.WithChiRoutes(r)))
	r.Use(cors.AllowAll().Handler)

	r.Get("/swagger/*", httpSwagger.Handler(
		httpSwagger.URL("https://unicode.fontpeek.com/swagger/doc.json"),
	))

	r.Get("/5.2.0/ucd", UCD)
	log.Println("Listening on :8080...")
	err := http.ListenAndServe(":8080", r)
	if err != nil {
		log.Fatal(err)
	}
}

// Unicode Character Directory
// @Tags         directory
// @Summary Query Unicode characters
// @ID unicode-character-directory
// @Produce  json
// @Param   cp query string false "Unicode Point hexadecimal (e.g. 0041)"
// @Param   fields query string false "Comma-separated list of fields to include in the response"
// @Param   limit query int false "How many results to include" minimum(1) maximum(100) default(5)
// @Param   offset query int false "Lookup offset" minimum(0)
// @Success 200 {string} string
// @Router /5.2.0/ucd [get]
func UCD(w http.ResponseWriter, r *http.Request) {
	fields := strings.Split(r.URL.Query().Get("fields"), ",")
	conn, err := pgPool.Conn(context.Background())
	if err != nil {
		fmt.Printf("Failed to query: %+v\n", err)
		return
	}
	codepoints := strings.Split(r.URL.Query().Get("cp"), ",")
	if len(codepoints) > 100 {
		codepoints = codepoints[:100]
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 32)
	if err != nil {
		offset = 0
	}
	limit, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 32)
	if err != nil || limit < 0 || limit > 100 {
		limit = 100
	}
	var glyphs *sql.Rows
	if r.URL.Query().Get("cp") == "" {
		if offset != 0 {
			glyphs, err = conn.QueryContext(context.Background(), "select * from glyphs offset $1 limit $2", offset, limit)
		} else {
			glyphs, err = conn.QueryContext(context.Background(), "select * from glyphs limit $1", limit)
		}
	} else {
		glyphs, err = conn.QueryContext(context.Background(), "select * from glyphs where cp = any($1)", pq.Array(codepoints))
	}
	if err != nil {
		fmt.Printf("Failed to query: %+v\n", err)
		return
	}
	result := []map[string]interface{}{}
	cols, _ := glyphs.Columns()
	validCols := intersection(cols, fields)
	if len(validCols) == 0 {
		validCols = cols
	}
	validColsMap := map[string]bool{}
	for _, field := range validCols {
		validColsMap[field] = true
	}
	for glyphs.Next() {
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i, _ := range columns {
			columnPointers[i] = &columns[i]
		}
		if err := glyphs.Scan(columnPointers...); err != nil {
			panic(err)
		}
		m := make(map[string]interface{})
		for i, colName := range cols {
			if !validColsMap[colName] {
				continue
			}
			val := columnPointers[i].(*interface{})
			m[colName] = *val
		}
		result = append(result, m)
	}
	conn.Close()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
