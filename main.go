package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/sqids/sqids-go"
	"github.com/wagslane/go-rabbitmq"
)

var sqidConfig *sqids.Sqids

//go:embed index.html
var indexHtml []byte

var imageListTemplate *template.Template

func init() {
	sqidConfig, _ = sqids.New(sqids.Options{MinLength: 10, Alphabet: "CS4LptvB7MmouJGAfqTaexczjk3PsWVhnZNd6QHDUERwb8y2XKrY95gF"})
	imageListTemplate, _ = template.New("").Parse(`
<table>
{{ range . }}
<tr><td><a href="/images/{{ index . 0 }}"><img src="/images/{{ index . 0 }}/thumbnail"/></a></td><td>{{ index . 0 }}</td><td>{{ index . 1 }}</td></tr>
{{ end }}
<table>
`)
}

func main() {
	if err := mainInner(); err != nil {
		slog.Error("exit with error", "error", err)
		os.Exit(1)
	}
}

func mainInner() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{})))

	amqpConnectionString := os.Getenv("AMQP_CONNECTION")
	if amqpConnectionString == "" {
		return errors.New("empty 'AMQP_CONNECTION'")
	}

	pgConnectionString := os.Getenv("POSTGRES_CONNECTION")
	if pgConnectionString == "" {
		return errors.New("empty 'POSTGRES_CONNECTION'")
	}

	slog.Info("connecting to AMQP", "conn", regexp.MustCompile("://.+@").ReplaceAllString(amqpConnectionString, "://<masked>@"))
	conn, err := rabbitmq.NewConn(
		amqpConnectionString,
		rabbitmq.WithConnectionOptionsLogging,
	)
	if err != nil {
		return errors.Wrap(err, "failed to connect to AMQP")
	}
	defer func() {
		slog.Info("closing AMQP")
		if err := conn.Close(); err != nil {
			slog.Error("error while closing AMQP", "error", err)
		}
	}()

	slog.Info("connecting to Postgres", "conn", regexp.MustCompile("://.+@").ReplaceAllString(pgConnectionString, "://<masked>@"))
	db, err := sql.Open("postgres", pgConnectionString)
	if err != nil {
		return errors.Wrap(err, "failed to connect to postgres")
	}
	defer func() {
		slog.Info("closing Postgres")
		if err := db.Close(); err != nil {
			slog.Error("error while closing postgres", "error", err)
		}
	}()

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS images (
 	id TEXT NOT NULL PRIMARY KEY,
 	created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,
 	image BYTEA NOT NULL,
 	image_format TEXT NOT NULL,
 	thumbnail BYTEA NULL
)
`); err != nil {
		return errors.Wrap(err, "failed to create initial tables")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(indexHtml)
	})

	mux.HandleFunc("GET /images/{id}/thumbnail", func(writer http.ResponseWriter, request *http.Request) {
		var out []byte
		if err := db.QueryRow(`SELECT thumbnail FROM images WHERE id = $1 AND thumbnail IS NOT NULL`, request.PathValue("id")).Scan(&out); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(http.StatusText(http.StatusNotFound)))
				return
			}
			slog.Error("failed to query image", "error", err)
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
			return
		}
		writer.Header().Set("Content-Type", "image/jpeg")
		_, _ = writer.Write(out)
	})

	mux.HandleFunc("GET /images/{id}", func(writer http.ResponseWriter, request *http.Request) {
		var out []byte
		var format string
		if err := db.QueryRow(`SELECT image, image_format FROM images WHERE id = $1`, request.PathValue("id")).Scan(&out, &format); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(http.StatusText(http.StatusNotFound)))
				return
			}
			slog.Error("failed to query image", "error", err)
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
			return
		}
		switch format {
		case "png":
			writer.Header().Set("Content-Type", "image/x-png")
		case "jpeg":
			writer.Header().Set("Content-Type", "image/jpeg")
		case "gif":
			writer.Header().Set("Content-Type", "image/gif")
		default:
			writer.Header().Set("Content-Type", "image/unknown")
		}
		_, _ = writer.Write(out)
	})

	mux.HandleFunc("GET /images/{$}", func(writer http.ResponseWriter, request *http.Request) {
		if res, err := db.Query(`SELECT id, created_at FROM images ORDER BY created_at DESC`); err != nil {
			slog.Error("failed to query images", "error", err)
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
		} else {
			rows := make([][2]string, 0)
			for res.Next() {
				id, createdAt := "", time.Time{}
				if err := res.Scan(&id, &createdAt); err != nil {
					slog.Error("failed to scan image", "error", err)
					writer.WriteHeader(http.StatusInternalServerError)
					_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
					return
				}
				rows = append(rows, [2]string{id, createdAt.Format(time.RFC1123)})
			}
			if res.Err() != nil {
				slog.Error("failed to scan images", "error", err)
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
				return
			}
			if err := imageListTemplate.Execute(writer, rows); err != nil {
				slog.Error("failed to template images", "error", err)
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
				return
			}
		}
	})

	mux.HandleFunc("POST /images/{$}", func(writer http.ResponseWriter, request *http.Request) {
		content, err := io.ReadAll(io.LimitReader(request.Body, 2e+7))
		if err != nil {
			slog.Error("failed to read request body", "error", err)
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusBadRequest)))
			return
		}
		mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil {
			slog.Error("failed to parse content type", "error", err)
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusBadRequest)))
			return
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(bytes.NewReader(content), params["boundary"])
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					slog.Error("failed to find 'file' in multipart form", "error", err)
					writer.WriteHeader(http.StatusBadRequest)
					_, _ = writer.Write([]byte(http.StatusText(http.StatusBadRequest)))
					return
				}
				if p.FormName() == "file" {
					content, _ = io.ReadAll(p)
					break
				}
			}
		}

		_, format, err := image.DecodeConfig(bytes.NewReader(content))
		if err != nil {
			slog.Error("failed to decode image from request body", "error", err, "size", len(content))
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte("failed to decode image from request body"))
			return
		}
		if _, ok := map[string]bool{"png": true, "jpeg": true, "gif": true}[format]; !ok {
			slog.Error("unsupported image format", "format", format, "size", len(content))
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte("unsupported image format"))
			return
		}

		id, _ := sqidConfig.Encode([]uint64{rand.Uint64()})
		if _, err := db.Exec(`INSERT INTO images (id, created_at, image, image_format) VALUES ($1, $2, $3, $4)`, id, time.Now().UTC(), content, format); err != nil {
			slog.Error("failed to insert image", "error", err)
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(http.StatusText(http.StatusInternalServerError)))
			return
		}
		slog.Info("uploaded image", "id", id)
		writer.Header().Set("HX-Redirect", "/")
		writer.WriteHeader(http.StatusCreated)
	})

	if err := http.ListenAndServe("0.0.0.0:8080", mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "failed to listen and server")
	}
	return nil
}
