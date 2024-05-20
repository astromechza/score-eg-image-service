package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
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
{{ range . }}
<div class="row">
	{{ range . }}
	<div class="column column-25">
	  <a href="/images/{{ index . 0 }}" title="{{ index . 0 }} - {{ index . 1 }} - {{ index . 2 }} -> {{ index . 3 }}">
		<img src="/images/{{ index . 0 }}/thumbnail"/>
	  </a>
	</div>
	{{ end }}
</div>
{{ else }}
<p>No images exist yet. Upload one now :)</p>
{{ end }}
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

	thumbnailGenerationRoutingKey := os.Getenv("AMQP_THUMBNAILING_ROUTING_KEY")
	if thumbnailGenerationRoutingKey == "" {
		return errors.New("empty 'thumbnailGenerationRoutingKey'")
	}
	thumbnailGeneratedRoutingKey := thumbnailGenerationRoutingKey + "-rcv"

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
 	image_size_bytes INT NOT NULL,
 	thumbnail BYTEA NULL,
 	thumbnail_size_bytes INT NOT NULL
)
`); err != nil {
		return errors.Wrap(err, "failed to create initial tables")
	}

	publisher, err := rabbitmq.NewPublisher(
		conn,
		rabbitmq.WithPublisherOptionsLogging,
	)
	if err != nil {
		return errors.Wrap(err, "failed to create publisher")
	}
	defer publisher.Close()

	consumer, err := rabbitmq.NewConsumer(
		conn,
		thumbnailGeneratedRoutingKey,
	)
	if err != nil {
		return errors.Wrap(err, "failed to bind consumer")
	}
	defer consumer.Close()

	broadcasters := make([]chan string, 0)
	broadcastLock := new(sync.Mutex)

	go func() {
		_ = consumer.Run(func(d rabbitmq.Delivery) (action rabbitmq.Action) {
			id := d.CorrelationId
			if id != "" {
				subLogger := slog.Default().With("id", id)
				_, _, err := image.DecodeConfig(bytes.NewReader(d.Body))
				if err == nil {
					res, err := db.Exec(`UPDATE images SET thumbnail = $1, thumbnail_size_bytes = $2 WHERE id = $3`, d.Body, len(d.Body), id)
					if err != nil {
						subLogger.Error("failed to insert thumbnail into database", "error", err)
						return rabbitmq.NackRequeue
					} else if c, _ := res.RowsAffected(); c > 0 {
						subLogger.Info("thumbnail set for image")
					} else {
						subLogger.Warn("no images matched id")
					}

					broadcastLock.Lock()
					defer broadcastLock.Unlock()
					for _, broadcaster := range broadcasters {
						broadcaster <- "ReloadImages"
					}
					subLogger.Info("sent event", "#broadcasters", len(broadcasters))
				} else if len(d.Body) > 100 {
					subLogger.Error("failed to parse thumbnail response", "error", err)
				} else {
					subLogger.Error("thumbnail response contains an error message", "error", string(d.Body))
				}
			}
			return rabbitmq.Ack
		})
	}()

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.GET("/alive", func(c echo.Context) error {
		return nil
	})

	e.GET("/ready", func(c echo.Context) error {
		return db.PingContext(c.Request().Context())
	})

	e.GET("/images/", func(c echo.Context) error {
		return c.HTMLBlob(http.StatusOK, indexHtml)
	})

	e.GET("/images/events", func(c echo.Context) error {
		w := c.Response()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		w.Flush()

		ch := make(chan string)
		func() {
			broadcastLock.Lock()
			defer broadcastLock.Unlock()
			broadcasters = append(broadcasters, ch)
			slog.Info("appended broadcaster", "#broadcasters", len(broadcasters))
		}()
		defer func() {
			broadcastLock.Lock()
			defer broadcastLock.Unlock()
			for i, broadcaster := range broadcasters {
				if broadcaster == ch {
					broadcasters[i] = broadcasters[len(broadcasters)-1]
					broadcasters = broadcasters[:len(broadcasters)-1]
					close(ch)
				}
			}
			slog.Info("removed broadcaster", "#broadcasters", len(broadcasters))
		}()

		for {
			select {
			case <-c.Request().Context().Done():
				return nil
			case s := <-ch:
				if _, err = fmt.Fprintf(w, "event: %s\ndata: \n\n", s); err != nil {
					return err
				}
				slog.Info("wrote chunk")
				w.Flush()
			}
		}
	})

	e.GET("/images/snippet", func(c echo.Context) error {
		if res, err := db.Query(`SELECT id, created_at, image_size_bytes, thumbnail_size_bytes FROM images ORDER BY created_at DESC`); err != nil {
			slog.Error("failed to query images", "error", err)
			return err
		} else {
			rows := make([][][4]string, 0)
			row := make([][4]string, 0)
			for res.Next() {
				id, createdAt, imageBytes, thumbnailBytes := "", time.Time{}, 0, 0
				if err := res.Scan(&id, &createdAt, &imageBytes, &thumbnailBytes); err != nil {
					slog.Error("failed to scan image", "error", err)
					return err
				}
				newRow := [4]string{id, createdAt.Format(time.RFC1123), ByteCountSI(int64(imageBytes)), ""}
				if thumbnailBytes > 0 {
					newRow[3] = ByteCountSI(int64(thumbnailBytes))
				}
				row = append(row, newRow)
				if len(row) == 4 {
					rows = append(rows, row)
					row = make([][4]string, 0)
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			if res.Err() != nil {
				slog.Error("failed to scan images", "error", err)
				return err
			}
			c.Response().Header().Set("Content-Type", echo.MIMETextHTMLCharsetUTF8)
			if err := imageListTemplate.Execute(c.Response().Writer, rows); err != nil {
				slog.Error("failed to template images", "error", err)
				return err
			}
			return nil
		}
	})

	e.POST("/images/", func(c echo.Context) error {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			return err
		}
		f, err := fileHeader.Open()
		if err != nil {
			return err
		}
		defer f.Close()
		content, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		_, format, err := image.DecodeConfig(bytes.NewReader(content))
		if err != nil {
			slog.Error("failed to decode image from request body", "error", err, "size", len(content))
			return c.String(http.StatusBadRequest, "failed to decode image from request body")
		}
		if _, ok := map[string]bool{"png": true, "jpeg": true, "gif": true}[format]; !ok {
			slog.Error("unsupported image format", "format", format, "size", len(content))
			return c.String(http.StatusBadRequest, "unsupported image format")
		}

		id, _ := sqidConfig.Encode([]uint64{rand.Uint64()})
		if _, err := db.Exec(`INSERT INTO images (id, created_at, image, image_format, image_size_bytes, thumbnail_size_bytes) VALUES ($1, $2, $3, $4, $5, 0)`, id, time.Now().UTC(), content, format, len(content)); err != nil {
			slog.Error("failed to insert image", "error", err)
			return err
		}
		slog.Info("uploaded image", "id", id)

		if err := publisher.Publish(
			content, []string{thumbnailGenerationRoutingKey},
			rabbitmq.WithPublishOptionsMessageID(id),
			rabbitmq.WithPublishOptionsReplyTo(thumbnailGeneratedRoutingKey),
		); err != nil {
			// TODO: since this is a demo I'm not handling errors here properly - in theory we should do this insice a transaction with the image
			return err
		}

		c.Response().Header().Set("HX-Trigger", "ReloadImages")
		c.Response().WriteHeader(http.StatusCreated)
		return nil
	})

	e.GET("/images/:id/thumbnail", func(c echo.Context) error {
		var out []byte
		if err := db.QueryRow(`SELECT thumbnail FROM images WHERE id = $1 AND thumbnail IS NOT NULL`, c.Param("id")).Scan(&out); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return c.String(http.StatusNotFound, http.StatusText(http.StatusNotFound))
			}
			slog.Error("failed to query image", "error", err)
			return err
		}
		return c.Blob(http.StatusOK, "image/jpeg", out)
	})

	e.GET("/images/:id", func(c echo.Context) error {
		var out []byte
		var format string
		if err := db.QueryRow(`SELECT image, image_format FROM images WHERE id = $1`, c.Param("id")).Scan(&out, &format); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return c.String(http.StatusNotFound, http.StatusText(http.StatusNotFound))
			}
			slog.Error("failed to query image", "error", err)
			return err
		}
		switch format {
		case "png":
			return c.Blob(http.StatusOK, "image/x-png", out)
		case "jpeg":
			return c.Blob(http.StatusOK, "image/jpeg", out)
		case "gif":
			return c.Blob(http.StatusOK, "image/gif", out)
		default:
			return c.Blob(http.StatusOK, "image/unknown", out)
		}
	})

	if err := http.ListenAndServe("0.0.0.0:8080", e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "failed to listen and server")
	}
	return nil
}

func ByteCountSI(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB",
		float64(b)/float64(div), "kMGTPE"[exp])
}
