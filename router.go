package huma

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"github.com/xeipuuv/gojsonschema"
	"go.uber.org/zap"
)

// ErrInvalidParamLocation is returned when the `in` field of the parameter
// is not a valid value.
var ErrInvalidParamLocation = errors.New("invalid parameter location")

// Checks if data validates against the given schema. Returns false on failure.
func validAgainstSchema(c *gin.Context, schema *Schema, data []byte) bool {
	loader := gojsonschema.NewGoLoader(schema)
	doc := gojsonschema.NewBytesLoader(data)
	s, err := gojsonschema.NewSchema(loader)
	if err != nil {
		panic(err)
	}
	result, err := s.Validate(doc)
	if err != nil {
		panic(err)
	}

	if !result.Valid() {
		errors := []string{}
		for _, desc := range result.Errors() {
			errors = append(errors, fmt.Sprintf("%s", desc))
		}
		c.AbortWithStatusJSON(400, &ErrorInvalidModel{
			Message: "Invalid input",
			Errors:  errors,
		})
		return false
	}

	return true
}

func getParamValue(c *gin.Context, param *Param) (interface{}, bool) {
	var pstr string
	switch param.In {
	case InPath:
		pstr = c.Param(param.Name)
	case InQuery:
		pstr = c.Query(param.Name)
		if pstr == "" {
			return param.def, true
		}
	case InHeader:
		pstr = c.GetHeader(param.Name)
		if pstr == "" {
			return param.def, true
		}
	default:
		panic(fmt.Errorf("%s: %w", param.In, ErrInvalidParamLocation))
	}

	if pstr == "" && !param.Required {
		// Optional and not passed, so set it to its zero value.
		return reflect.New(param.typ).Elem().Interface(), true
	}

	if param.Schema.HasValidation() {
		data := pstr
		if param.Schema.Type == "string" {
			// Strings are special in that we don't expect users to provide them
			// with quotes, so wrap them here for the parser that does the
			// validation step below.
			data = `"` + data + `"`
		}
		if !validAgainstSchema(c, param.Schema, []byte(data)) {
			return nil, false
		}
	}

	var pv interface{}
	switch param.typ.Kind() {
	case reflect.Bool:
		converted, err := strconv.ParseBool(pstr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, &ErrorModel{
				Message: fmt.Sprintf("cannot parse boolean for param %s: %s", param.Name, pstr),
			})
			return nil, false
		}
		pv = converted
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		converted, err := strconv.Atoi(pstr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, &ErrorModel{
				Message: fmt.Sprintf("cannot parse integer for param %s: %s", param.Name, pstr),
			})
			return nil, false
		}
		pv = reflect.ValueOf(converted).Convert(param.typ).Interface()
	case reflect.Float32:
		converted, err := strconv.ParseFloat(pstr, 32)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, &ErrorModel{
				Message: fmt.Sprintf("cannot parse float for param %s: %s", param.Name, pstr),
			})
			return nil, false
		}
		pv = converted
	case reflect.Float64:
		converted, err := strconv.ParseFloat(pstr, 64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, &ErrorModel{
				Message: fmt.Sprintf("cannot parse float for param %s: %s", param.Name, pstr),
			})
			return nil, false
		}
		pv = converted
	default:
		pv = pstr
	}

	return pv, true
}

func getRequestBody(c *gin.Context, t reflect.Type, op *Operation) (interface{}, bool) {
	val := reflect.New(t).Interface()
	if op.RequestSchema != nil {
		body, err := ioutil.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithError(500, err)
			return nil, false
		}

		c.Request.Body = ioutil.NopCloser(bytes.NewBuffer(body))

		if !validAgainstSchema(c, op.RequestSchema, body) {
			return nil, false
		}
	}

	if err := c.ShouldBindJSON(val); err != nil {
		c.AbortWithError(500, err)
		return nil, false
	}

	return val, true
}

// Router handles API requests.
type Router struct {
	api         *OpenAPI
	engine      *gin.Engine
	root        *cobra.Command
	prestart    []func()
	docsHandler func(c *gin.Context, api *OpenAPI)
}

// NewRouter creates a new Huma router for handling API requests with
// default middleware and routes attached. This is equivalent to calling
// `NewRouterWithGin` with a new Gin instance with just the recovery,
// CORS (allowing all origins), and log middlewares.
func NewRouter(api *OpenAPI) *Router {
	g := gin.New()
	g.Use(Recovery())
	g.Use(LogMiddleware(nil, nil))
	g.Use(cors.Default())
	g.NoRoute(Handler404)
	return NewRouterWithGin(g, api)
}

// NewRouterWithGin creates a new Huma router with the given Gin instance
// which may be preconfigured with custom options and middleware.
func NewRouterWithGin(engine *gin.Engine, api *OpenAPI) *Router {
	if err := api.validate(); err != nil {
		panic(err)
	}

	r := &Router{
		api:         api,
		engine:      engine,
		prestart:    []func(){},
		docsHandler: RapiDocHandler,
	}

	r.setupCLI()

	if r.api.Paths == nil {
		r.api.Paths = make(map[string]map[string]*Operation)
	}

	// Set up handlers for the auto-generated spec and docs.
	r.engine.GET("/openapi.json", OpenAPIHandler(r.api))

	r.engine.GET("/docs", func(c *gin.Context) {
		r.docsHandler(c, api)
	})

	// If downloads like a CLI or SDKs are available, serve them automatically
	// so you can reference them from e.g. the docs.
	if _, err := os.Stat("downloads"); err == nil {
		r.engine.Static("/downloads", "downloads")
	}

	return r
}

// GinEngine returns the underlying low-level Gin engine.
func (r *Router) GinEngine() *gin.Engine {
	return r.engine
}

// PreStart registers a function to run before server start.
func (r *Router) PreStart(f func()) {
	r.prestart = append(r.prestart, f)
}

// SetDocsHandler sets the documentation rendering handler function. You can
// use `huma.RapiDocHandler`, `huma.ReDocHandler`, `huma.SwaggerUIHandler`, or
// provide your own (e.g. with custom auth or branding).
func (r *Router) SetDocsHandler(f func(*gin.Context, *OpenAPI)) {
	r.docsHandler = f
}

// Use attaches middleware to the router.
func (r *Router) Use(middleware ...gin.HandlerFunc) {
	r.engine.Use(middleware...)
}

// ServeHTTP conforms to the `http.Handler` interface.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.engine.ServeHTTP(w, req)
}

// Resource creates a new resource at the given path with the given
// dependencies, parameters, response headers, and responses defined.
func (r *Router) Resource(path string, depsParamsHeadersOrResponses ...interface{}) *Resource {
	return NewResource(r, path).With(depsParamsHeadersOrResponses...)
}

// Register a new operation.
func (r *Router) Register(method, path string, op *Operation) {
	// First, make sure the operation and handler make sense, as well as pre-
	// generating any schemas for use later during request handling.
	if err := op.validate(method, path); err != nil {
		panic(err)
	}

	// Add the operation to the list of operations for the path entry.
	if r.api.Paths[path] == nil {
		r.api.Paths[path] = make(map[string]*Operation)
	}

	r.api.Paths[path][method] = op

	// Next, figure out which Gin function to call.
	var f func(string, ...gin.HandlerFunc) gin.IRoutes

	switch method {
	case "OPTIONS":
		f = r.engine.OPTIONS
	case "HEAD":
		f = r.engine.HEAD
	case "GET":
		f = r.engine.GET
	case "POST":
		f = r.engine.POST
	case "PUT":
		f = r.engine.PUT
	case "PATCH":
		f = r.engine.PATCH
	case "DELETE":
		f = r.engine.DELETE
	default:
		panic("unsupported HTTP method " + method)
	}

	if strings.Contains(path, "{") {
		// Convert from OpenAPI-style parameters to gin-style params
		path = paramRe.ReplaceAllString(path, ":$1$2")
	}

	// Then call it to register our handler function.
	f(path, func(c *gin.Context) {
		method := reflect.ValueOf(op.Handler)
		in := make([]reflect.Value, 0, method.Type().NumIn())

		// Process any dependencies first.
		for _, dep := range op.Dependencies {
			headers, value, err := dep.Resolve(c, op)
			if err != nil {
				// TODO: better error handling
				if !c.IsAborted() {
					// Nothing else has handled the error, so treat it like a general
					// internal server error.
					c.AbortWithStatusJSON(500, &ErrorModel{
						Message: "Couldn't get dependency",
						//Errors:  []error{err},
					})
				}
			}

			for k, v := range headers {
				c.Header(k, v)
			}

			in = append(in, reflect.ValueOf(value))
		}

		for _, param := range op.Params {
			pv, ok := getParamValue(c, param)
			if !ok {
				// Error has already been handled.
				return
			}

			in = append(in, reflect.ValueOf(pv))
		}

		if len(in) != method.Type().NumIn() {
			// Parse body
			i := len(in)
			val, success := getRequestBody(c, method.Type().In(i), op)
			if !success {
				// Error was already handled in `getRequestBody`.
				return
			}
			in = append(in, reflect.ValueOf(val))
			if in[i].Kind() == reflect.Ptr {
				in[i] = in[i].Elem()
			}
		}

		out := method.Call(in)

		// Find and return the first non-zero response. The status code comes
		// from the registered `huma.Response` struct.
		// This breaks down with scalar types... so they need to be passed
		// as a pointer and we'll dereference it automatically.
		for i, o := range out[len(op.ResponseHeaders):] {
			if !o.IsZero() {
				body := o.Interface()

				r := op.Responses[i]

				// Set response headers
				for j, header := range op.ResponseHeaders {
					value := out[j]

					found := false
					for _, name := range r.Headers {
						if name == header.Name {
							found = true
							break
						}
					}

					if !found {
						if !value.IsZero() {
							// Log an error to be fixed later if using the logging middleware.
							if l, ok := c.Get("log"); ok {
								if log, ok := l.(*zap.SugaredLogger); ok {
									log.Errorf("Header '%s' with value '%v' set on a response that did not declare it", header.Name, value)
								}
							}
						}
						// Skip this header as the response doesn't list it.
						continue
					}

					if !value.IsZero() {
						v := value.Interface()
						if value.Kind() == reflect.Ptr {
							v = value.Elem().Interface()
						}
						c.Header(header.Name, fmt.Sprintf("%v", v))
					}
				}

				if r.empty {
					// No body allowed, e.g. for HTTP 204.
					c.Status(r.StatusCode)
					break
				}

				if strings.HasPrefix(r.ContentType, "application/json") {
					c.JSON(r.StatusCode, body)
				} else if strings.HasPrefix(r.ContentType, "application/yaml") {
					c.YAML(r.StatusCode, body)
				} else {
					if o.Kind() == reflect.Ptr {
						// This is a pointer to something, so we derefernce it and get
						// its value before converting to a string because Printf will
						// by default print pointer addresses instead of their value.
						body = o.Elem().Interface()
					}
					c.Data(r.StatusCode, r.ContentType, []byte(fmt.Sprintf("%v", body)))
				}
				break
			}
		}
	})
}

// Listen for new connections.
func (r *Router) Listen(addr string) error {
	return r.engine.Run(addr)
}

// ListenTLS listens for new connections using HTTPS & HTTP2
func (r *Router) ListenTLS(addr, certFile, keyFile string) error {
	return r.engine.RunTLS(addr, certFile, keyFile)
}

// Run executes the router command.
func (r *Router) Run() {
	if err := r.root.Execute(); err != nil {
		panic(err)
	}
}
