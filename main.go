package avengers

import (
	"bytes"
	"encoding/json"
	"fmt"
	htmlR "html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
	//"time"

	"go.chromium.org/gae/impl/prod"
	//"go.chromium.org/gae/service/datastore"
	"go.chromium.org/gae/service/memcache"
	"go.chromium.org/gae/service/urlfetch"

	"go.chromium.org/luci/common/logging"

	"golang.org/x/net/context"
	"golang.org/x/net/html"
)

func init() {
	http.HandleFunc("/", getIndex)
	http.HandleFunc("/movies", getMCUMovieData)
	http.HandleFunc("/release", getIWRelease)
}

var tpl = template.Must(template.ParseGlob("static/templates/*.html"))

func getIndex(w http.ResponseWriter, r *http.Request) {
	if err := tpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		panic(err)
	}

}

type nodePred func(*html.Node) bool

func hasChildId(id string) nodePred {
	return func(n *html.Node) bool {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			for _, attr := range c.Attr {
				if attr.Key == "id" && attr.Val == id {
					return true
				}
			}
		}
		return false
	}
}

func findNode(n *html.Node, nodeType string, pred nodePred) *html.Node {
	if n.Type == html.ElementNode && n.Data == nodeType {
		if pred(n) {
			return n
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if res := findNode(c, nodeType, pred); res != nil {
			return res
		}
	}
	return nil
}

func nodeChildren(n *html.Node) []*html.Node {
	arr := []*html.Node{}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		arr = append(arr, c)
	}
	return arr

}

// n is assumed to be a table
// This will break if not used on film.
func getHTMLTableColumn(c context.Context, n *html.Node, column string) []*html.Node {
	// There's a weird newline we have to skip
	n = n.FirstChild.NextSibling
	if n.Data != "tbody" {
		panic(fmt.Sprintf("bad node %v %v", n, textContent(n)))
	}
	vals := []*html.Node{}
	idx := -1
	// First child should be a header row
	for i, cc := range nodeChildren(n.FirstChild) {
		if strings.HasPrefix(textContent(cc), column) {
			idx = i
			break
		}
	}

	for ch := n.FirstChild.NextSibling; ch != nil; ch = ch.NextSibling {
		itms := nodeChildren(ch)
		if len(itms) == 0 {
			continue
		}
		vals = append(vals, itms[idx])
	}
	return vals
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	s := ""
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s += textContent(c)
	}
	if len(s) > 0 {
		return s
	}
	return ""
}

func getAttr(attrs []html.Attribute, name string) string {
	for _, a := range attrs {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func getMovies(c context.Context, doc *html.Node, phaseIds ...string) []*Movie {
	movies := []*Movie{}
	for _, phaseId := range phaseIds {
		phaneNode := findNode(doc, "h2", hasChildId(phaseId))
		tblNode := phaneNode.NextSibling
		if tblNode.Type == html.TextNode {
			tblNode = tblNode.NextSibling
		}
		tableVal := getHTMLTableColumn(c, tblNode, "Film")
		for _, n := range tableVal {
			name := textContent(n)
			linkNode := n.FirstChild.FirstChild
			movies = append(movies, &Movie{
				Name:    name,
				Phase:   strings.Replace(phaseId, "_", " ", -1),
				Runtime: getRuntime(c, name, getAttr(linkNode.Attr, "href")),
			})
		}
	}
	return movies
}

func getRuntime(c context.Context, title, href string) int {
	// Fake for now, lazy about getting these values
	//return 120
	itm := memcache.NewItem(c, fmt.Sprintf("runtime|%v", title))
	err := memcache.Get(c, itm)
	isCacheMiss := err == memcache.ErrCacheMiss
	if err != nil {
		doc, err := getWebpage(c, "https://en.wikipedia.org"+href)
		if err != nil {
			panic(err)
		}
		phaneNode := findNode(doc, "tr", func(n *html.Node) bool {
			if n.FirstChild == nil {
				return false
			}
			n = n.FirstChild.NextSibling
			if n == nil || n.FirstChild == nil || n.Data != "th" {
				return false
			}
			n = n.FirstChild.NextSibling
			if n == nil {
				return false
			}
			return strings.TrimSpace(textContent(n)) == "Running time"
		})
		valNode := phaneNode.FirstChild.NextSibling.NextSibling.NextSibling.FirstChild
		logging.Warningf(c, "found %v", textContent(valNode))
		split := strings.Split(valNode.Data, " ")
		if split[1] != "minutes" {
			panic(fmt.Sprintf("weird %v", split))
		}
		val, err := strconv.Atoi(split[0])
		if err != nil {
			panic(err)
		}
		itm.SetValue([]byte{byte(val)})
		if isCacheMiss {
			err := memcache.Set(c, itm)
			if err != nil {
				panic(err)
			}
		}
	}
	return int(itm.Value()[0])

}

type Movie struct {
	Name    string `json:"name"`
	Runtime int    `json:"runtime"`
	Phase   string `json:"phase"`
}

func getMCUMovieData(w http.ResponseWriter, r *http.Request) {
	c := context.Background()
	c = prod.Use(c, r)

	movies := []*Movie{}
	itm := memcache.NewItem(c, "mcumovies")
	err := memcache.Get(c, itm)
	if err != memcache.ErrCacheMiss && err != nil {
		panic(err)
	}
	if err == memcache.ErrCacheMiss {
		doc, err := getWebpage(c, "https://en.wikipedia.org/wiki/List_of_Marvel_Cinematic_Universe_films")
		if err != nil {
			panic(err)
		}
		movies = getMovies(c, doc, "Phase_One", "Phase_Two", "Phase_Three")
		res, err := json.Marshal(movies)
		if err != nil {
			panic(err)
		}
		itm.SetValue(res)
		//err = memcache.Set(c, itm)
		//if err != nil {
		//panic(err)
		//}
	}
	w.WriteHeader(200)
	w.Write(itm.Value())
}

func getWebpage(c context.Context, url string) (*html.Node, error) {
	rt := urlfetch.Get(c)

	req, err := http.NewRequest("GET", url, bytes.NewReader(nil))
	if err != nil {
		panic(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		panic(err)
	}

	return html.Parse(resp.Body)
}

func getIWRelease(w http.ResponseWriter, r *http.Request) {
	c := context.Background()
	c = prod.Use(c, r)

	itm := memcache.NewItem(c, "releasedate")
	err := memcache.Get(c, itm)
	if err != memcache.ErrCacheMiss && err != nil {
		panic(err)
	}
	if err == memcache.ErrCacheMiss {
		doc, err := getWebpage(c, "https://en.wikipedia.org/wiki/Avengers:_Infinity_War")
		phaneNode := findNode(doc, "tr", func(n *html.Node) bool {
			if n.FirstChild == nil {
				return false
			}
			n = n.FirstChild.NextSibling
			if n == nil || n.FirstChild == nil || n.Data != "th" {
				return false
			}
			n = n.FirstChild.NextSibling
			if n == nil {
				return false
			}
			return strings.TrimSpace(textContent(n)) == "Release date"
		})
		// :)
		dateNode := phaneNode.FirstChild.NextSibling.NextSibling.NextSibling.FirstChild.NextSibling.FirstChild.NextSibling.FirstChild.NextSibling
		dateNode = dateNode.FirstChild
		txt := htmlR.UnescapeString(textContent(dateNode))
		logging.Warningf(c, "found %v", txt)
		txt = string(bytes.Replace([]byte(txt), []byte{194, 160}, []byte(nil), -1))
		logging.Warningf(c, "found %v", txt)
		const longForm = "Jan2,2006"
		t, err := time.Parse(longForm, txt)
		if err != nil {
			panic(err)
		}
		logging.Warningf(c, "got %v", t)
		d, err := t.MarshalJSON()
		if err != nil {
			panic(err)
		}
		itm.SetValue(d)
		err = memcache.Set(c, itm)
		if err != nil {
			panic(err)
		}
	}

	w.WriteHeader(200)
	w.Write(itm.Value())
}
