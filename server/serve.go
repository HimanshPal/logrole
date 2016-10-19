// Package server responds to incoming HTTP requests and renders the site.
//
// There are a number of smaller servers in this package, each of which takes
// only the configuration necessary to serve it.
package server

import (
	"bytes"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kevinburke/handlers"
	"github.com/kevinburke/rest"
	twilio "github.com/kevinburke/twilio-go"
	"github.com/saintpete/logrole/assets"
	"github.com/saintpete/logrole/config"
	"github.com/saintpete/logrole/services"
	"github.com/saintpete/logrole/views"
)

const Version = "0.13"

var year = time.Now().UTC().Year()

var funcMap = template.FuncMap{
	"year":          func() int { return year },
	"friendly_date": services.FriendlyDate,
	"duration":      services.Duration,
	"render":        renderTime,
}

// renderTime returns the amount of time since we began rendering this
// template; it's designed to approximate the amount of time spent in the
// render phase on the server.
func renderTime(start time.Time) string {
	since := time.Since(start)
	return services.Duration(since)
}

func init() {
	staticServer = &static{
		modTime: time.Now().UTC(),
	}

	base := string(assets.MustAsset("templates/base.html"))
	templates := template.Must(template.New("base").Option("missingkey=error").Funcs(funcMap).Parse(base))

	tindex := template.Must(templates.Clone())
	indexTpl := string(assets.MustAsset("templates/index.html"))
	indexTemplate = template.Must(tindex.Parse(indexTpl))
}

type static struct {
	modTime time.Time
}

func AuthUserHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, _, err := config.AuthUser(r)
		if err != nil {
			rest.Forbidden(w, r, &rest.Error{
				Title: err.Error(),
			})
			return
		}
		h.ServeHTTP(w, r)
	})
}

func UpgradeInsecureHandler(h http.Handler, allowUnencryptedTraffic bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowUnencryptedTraffic == false {
			if r.Header.Get("X-Forwarded-Proto") == "http" {
				u := r.URL
				u.Scheme = "https"
				u.Host = r.Host
				http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
				return
			}
		}
		// This header doesn't mean anything when served over HTTP, but
		// detecting HTTPS is a general way is hard, so let's just send it
		// every time.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		h.ServeHTTP(w, r)
	})
}

var staticServer http.Handler

func (s *static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bits, err := assets.Asset(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		rest.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, r.URL.Path, s.modTime, bytes.NewReader(bits))
}

type baseData struct {
	Duration time.Duration
	Start    time.Time
}

type indexServer struct{}

type indexData struct {
	baseData
}

var indexTemplate *template.Template

func (i *indexData) Title() string {
	return "Logrole Homepage"
}

func (i *indexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := &indexData{
		baseData: baseData{
			Duration: 0,
			Start:    time.Now(),
		},
	}
	if err := render(w, indexTemplate, "base", data); err != nil {
		rest.ServerError(w, r, err)
	}
}

// Settings are used to configure a Server and apply to all of the website's
// users.
type Settings struct {
	// The host the user visits to get to this site.
	PublicHost              string
	AllowUnencryptedTraffic bool
	Users                   map[string]string
	Client                  *twilio.Client

	// Times will be displayed in this Location.
	Location *time.Location

	// How many messages to display per page.
	PageSize uint

	// Used to encrypt next page URI's and sessions. See config.sample.yml for
	// more information.
	SecretKey *[32]byte

	// Don't show resources that are older than this age.
	MaxResourceAge time.Duration

	// Should a user have to click a button to view media attached to a MMS?
	ShowMediaByDefault bool
}

// TODO add different users, or pull from database
var theUser = config.NewUser(config.AllUserSettings())

// NewServer returns a new Handler that can serve the website. If the
// settings.Users map is empty, Basic Authentication is disabled.
func NewServer(settings *Settings) http.Handler {
	validKey := false
	for i := 0; i < len(settings.SecretKey); i++ {
		if settings.SecretKey[i] != 0x0 {
			validKey = true
			break
		}
	}
	if !validKey {
		panic("server: nil secret key in settings")
	}

	// TODO persistent storage
	for name := range settings.Users {
		config.AddUser(name, theUser)
	}

	permission := config.NewPermission(settings.MaxResourceAge)
	vc := views.NewClient(handlers.Logger, settings.Client, settings.SecretKey, permission)
	mls := &messageListServer{
		Client:         vc,
		Location:       settings.Location,
		PageSize:       settings.PageSize,
		SecretKey:      settings.SecretKey,
		MaxResourceAge: settings.MaxResourceAge,
	}
	mis := &messageInstanceServer{
		Client:             vc,
		Location:           settings.Location,
		ShowMediaByDefault: settings.ShowMediaByDefault,
	}
	cls := &callListServer{
		Client:         vc,
		Location:       settings.Location,
		SecretKey:      settings.SecretKey,
		PageSize:       settings.PageSize,
		MaxResourceAge: settings.MaxResourceAge,
	}
	ss := &searchServer{}
	o := &openSearchXMLServer{
		PublicHost:              settings.PublicHost,
		AllowUnencryptedTraffic: settings.AllowUnencryptedTraffic,
	}
	index := &indexServer{}
	image := &imageServer{
		SecretKey: settings.SecretKey,
	}
	r := new(handlers.Regexp)
	r.Handle(regexp.MustCompile(`^/$`), []string{"GET"}, index)
	r.Handle(imageRoute, []string{"GET"}, image)
	r.Handle(regexp.MustCompile(`^/search$`), []string{"GET"}, ss)
	r.Handle(regexp.MustCompile(`^/opensearch.xml$`), []string{"GET"}, o)
	r.Handle(regexp.MustCompile(`^/messages$`), []string{"GET"}, mls)
	r.Handle(regexp.MustCompile(`^/calls$`), []string{"GET"}, cls)
	r.Handle(messageInstanceRoute, []string{"GET"}, mis)
	r.Handle(regexp.MustCompile(`^/static`), []string{"GET"}, staticServer)
	var h http.Handler = UpgradeInsecureHandler(r, settings.AllowUnencryptedTraffic)
	if len(settings.Users) > 0 {
		// TODO database, remove duplication
		h = AuthUserHandler(h)
		h = handlers.BasicAuth(h, "logrole", settings.Users)
	}
	return handlers.Duration(
		handlers.Log(
			handlers.Debug(
				handlers.TrailingSlashRedirect(
					handlers.UUID(
						handlers.Server(h, "logrole/"+Version),
					),
				),
			),
		),
	)
}
