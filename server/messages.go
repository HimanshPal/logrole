package server

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/inconshreveable/log15"
	types "github.com/kevinburke/go-types"
	"github.com/kevinburke/rest"
	twilio "github.com/kevinburke/twilio-go"
	"github.com/saintpete/logrole/assets"
	"github.com/saintpete/logrole/config"
	"github.com/saintpete/logrole/services"
	"github.com/saintpete/logrole/views"
)

var messageInstanceTemplate *template.Template
var messageListTemplate *template.Template

const messagePattern = `(?P<sid>(MM|SM)[a-f0-9]{32})`

var messageInstanceRoute = regexp.MustCompile("^/messages/" + messagePattern + "$")

func init() {
	base := string(assets.MustAsset("templates/base.html"))
	templates := template.Must(template.New("base").Option("missingkey=error").Funcs(funcMap).Parse(base))
	phoneTpl := string(assets.MustAsset("templates/snippets/phonenumber.html"))

	tlist := template.Must(templates.Clone())
	listTpl := string(assets.MustAsset("templates/messages/list.html"))
	copyScript := string(assets.MustAsset("templates/snippets/copy-phonenumber.js"))
	messageListTemplate = template.Must(tlist.Parse(listTpl + phoneTpl + copyScript))

	tinstance := template.Must(templates.Clone())
	instanceTpl := string(assets.MustAsset("templates/messages/instance.html"))
	messageInstanceTemplate = template.Must(tinstance.Parse(instanceTpl + phoneTpl + copyScript))
}

type messageInstanceServer struct {
	log.Logger
	Client             views.Client
	LocationFinder     services.LocationFinder
	ShowMediaByDefault bool
}

type messageInstanceData struct {
	Message            *views.Message
	Loc                *time.Location
	Media              *mediaResp
	ShowMediaByDefault bool
}

func (m *messageInstanceData) Title() string {
	return "Message Details"
}

type mediaResp struct {
	Err  error
	URLs []*url.URL
}

func (s *messageInstanceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, ok := config.GetUser(r)
	if !ok {
		rest.ServerError(w, r, errors.New("No user available"))
		return
	}
	sid := messageInstanceRoute.FindStringSubmatch(r.URL.Path)[1]
	start := time.Now()
	rch := make(chan *mediaResp, 1)
	go func(sid string) {
		urls, err := s.Client.GetMediaURLs(u, sid)
		rch <- &mediaResp{
			URLs: urls,
			Err:  err,
		}
		close(rch)
	}(sid)
	message, err := s.Client.GetMessage(u, sid)
	switch err {
	case nil:
		break
	case config.PermissionDenied, config.ErrTooOld:
		rest.Forbidden(w, r, &rest.Error{Title: err.Error()})
		return
	default:
		switch terr := err.(type) {
		case *rest.Error:
			switch terr.StatusCode {
			case 404:
				rest.NotFound(w, r)
			default:
				rest.ServerError(w, r, terr)
			}
		default:
			rest.ServerError(w, r, err)
		}
		return
	}
	if !message.CanViewProperty("Sid") {
		rest.Forbidden(w, r, &rest.Error{Title: "Cannot view this message"})
		return
	}
	baseData := &baseData{LF: s.LocationFinder, Duration: time.Since(start)}
	data := &messageInstanceData{
		Message:            message,
		Loc:                s.LocationFinder.GetLocationReq(r),
		ShowMediaByDefault: s.ShowMediaByDefault,
	}
	numMedia, err := message.NumMedia()
	switch {
	case err != nil:
		data.Media = &mediaResp{Err: err}
	case numMedia > 0:
		r := <-rch
		data.Media = r
	}
	baseData.Data = data
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := render(w, r, messageInstanceTemplate, "base", baseData); err != nil {
		rest.ServerError(w, r, err)
	}
}

type messageListServer struct {
	log.Logger
	Client         views.Client
	LocationFinder services.LocationFinder
	PageSize       uint
	SecretKey      *[32]byte
	MaxResourceAge time.Duration
}

type messageData struct {
	Page              *views.MessagePage
	EncryptedNextPage string
	Loc               *time.Location
	Query             url.Values
	Err               string
	MaxResourceAge    time.Duration
}

func (m *messageData) Title() string {
	return "Messages"
}

// Min returns the minimum acceptable resource date, formatted for use in a
// date HTML input field.
func (m *messageData) Min() string {
	return time.Now().Add(-m.MaxResourceAge).Format("2006-01-02")
}

// Max returns a the maximum acceptable resource date, formatted for use in a
// date HTML input field.
func (m *messageData) Max() string {
	return time.Now().UTC().Format("2006-01-02")
}

func (s *messageListServer) renderError(w http.ResponseWriter, r *http.Request, code int, query url.Values, err error) {
	if err == nil {
		panic("called renderError with a nil error")
	}
	str := strings.Replace(err.Error(), "twilio: ", "", 1)
	data := &baseData{LF: s.LocationFinder,
		Data: &messageData{
			Err:            str,
			Query:          query,
			Page:           new(views.MessagePage),
			MaxResourceAge: s.MaxResourceAge,
		}}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := render(w, r, messageListTemplate, "base", data); err != nil {
		rest.ServerError(w, r, err)
	}
}

func (s *messageListServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, ok := config.GetUser(r)
	if !ok {
		rest.ServerError(w, r, errors.New("No user available"))
		return
	}
	if !u.CanViewMessages() {
		rest.Forbidden(w, r, &rest.Error{Title: "Access denied"})
		return
	}
	// This is modified as we parse the query; specifically we add some values
	// if they are present in the next page URI.
	query := r.URL.Query()
	page := new(views.MessagePage)
	var err error
	opaqueNext := query.Get("next")
	start := time.Now()
	if opaqueNext != "" {
		next, nextErr := services.Unopaque(opaqueNext, s.SecretKey)
		if nextErr != nil {
			err = errors.New("Could not decrypt `next` query parameter: " + nextErr.Error())
			s.renderError(w, r, http.StatusBadRequest, query, err)
			return
		}
		if !strings.HasPrefix(next, "/"+twilio.APIVersion) {
			s.Warn("Invalid next page URI", "next", next, "opaque", opaqueNext)
			s.renderError(w, r, http.StatusBadRequest, query, errors.New("Invalid next page uri"))
			return
		}
		page, err = s.Client.GetNextMessagePage(u, next)
		setNextPageValsOnQuery(next, query)
	} else {
		// valid values: https://www.twilio.com/docs/api/rest/message#list
		data := url.Values{}
		data.Set("PageSize", strconv.FormatUint(uint64(s.PageSize), 10))
		if filterErr := setPageFilters(query, data); filterErr != nil {
			s.renderError(w, r, http.StatusBadRequest, query, filterErr)
			return
		}
		page, err = s.Client.GetMessagePage(u, data)
	}
	if err != nil {
		switch terr := err.(type) {
		case *rest.Error:
			switch terr.StatusCode {
			case 400:
				s.renderError(w, r, http.StatusBadRequest, query, err)
			case 404:
				rest.NotFound(w, r)
			default:
				rest.ServerError(w, r, terr)
			}
		default:
			rest.ServerError(w, r, err)
		}
		return
	}
	// Fetch the next page into the cache
	go func(u *config.User, n types.NullString) {
		if n.Valid {
			if _, err := s.Client.GetNextMessagePage(u, n.String); err != nil {
				s.Debug("Error fetching next page", "err", err)
			}
		}
	}(u, page.NextPageURI())
	data := &baseData{
		LF: s.LocationFinder,
		Data: &messageData{
			Page:              page,
			Loc:               s.LocationFinder.GetLocationReq(r),
			Query:             query,
			MaxResourceAge:    s.MaxResourceAge,
			EncryptedNextPage: getEncryptedNextPage(page.NextPageURI(), s.SecretKey),
		}, Duration: time.Since(start)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := render(w, r, messageListTemplate, "base", data); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, query, err)
		return
	}
}
