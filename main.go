package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/extemporalgenome/slug"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"html/template"
	"io"
	"reflect"
	"strings"
)

var templates, _ = template.ParseGlob(
	"admin/templates/*.html",
)

type Admin struct {
	Router        *mux.Router
	Path          string
	Database      string
	Title         string
	NameTransform func(string) string

	Username string
	Password string
	sessions map[string]*session

	db          *sql.DB
	models      map[string]*model
	modelGroups []*modelGroup
}

// Setup registers page handlers and enables the admin.
func Setup(admin *Admin) (*Admin, error) {
	if len(admin.Title) == 0 {
		admin.Title = "Admin"
	}

	if len(admin.Username) == 0 || len(admin.Password) == 0 {
		return nil, errors.New("Username and/or password is missing")
	}

	admin.sessions = map[string]*session{}

	db, err := sql.Open("sqlite3", admin.Database)
	if err != nil {
		return nil, err
	}
	admin.db = db

	admin.models = map[string]*model{}
	admin.modelGroups = []*modelGroup{}

	sr := admin.Router.PathPrefix(admin.Path).Subrouter()
	sr.StrictSlash(true)
	sr.HandleFunc("/", admin.handlerWrapper(admin.handleIndex))
	sr.HandleFunc("/logout/", admin.handlerWrapper(admin.handleLogout))
	sr.HandleFunc("/model/{slug}/", admin.handlerWrapper(admin.handleList))
	sr.HandleFunc("/model/{slug}/new/", admin.handlerWrapper(admin.handleEdit))
	sr.HandleFunc("/model/{slug}/edit/{id}/", admin.handlerWrapper(admin.handleEdit))
	return admin, nil
}

// Group adds a model group to the admin front page.
// Use this to organize your models.
func (a *Admin) Group(name string) (*modelGroup, error) {
	if a.models == nil {
		return nil, errors.New("Must call .Serve() before adding groups and registering models")
	}

	group := &modelGroup{
		admin:  a,
		Name:   name,
		slug:   slug.SlugAscii(name),
		Models: []*model{},
	}

	a.modelGroups = append(a.modelGroups, group)

	return group, nil
}

type modelGroup struct {
	admin  *Admin
	Name   string
	slug   string
	Models []*model
}

type namedModel interface {
	AdminName() string
}

// RegisterModel adds a model to a model group.
func (g *modelGroup) RegisterModel(mdl interface{}) error {
	t := reflect.TypeOf(mdl)

	val := reflect.ValueOf(mdl)
	ind := reflect.Indirect(val)

	parts := strings.Split(t.String(), ".")
	name := parts[len(parts)-1]

	var tableName string
	if g.admin.NameTransform != nil {
		tableName = g.admin.NameTransform(name)
	} else {
		tableName = name
	}

	if named, ok := mdl.(namedModel); ok {
		name = named.AdminName()
	}

	am := model{
		Name:      name,
		Slug:      slug.SlugAscii(name),
		tableName: tableName,
		fields:    []Field{},
		instance:  mdl,
	}

	for i := 0; i < ind.NumField(); i++ {
		refl := t.Elem().Field(i)
		fieldType := refl.Type
		kind := fieldType.Kind()
		tag := refl.Tag.Get("admin")
		if tag == "-" {
			continue
		}

		// Parse key=val / key options from struct tag, used for configuration later
		tagMap, err := parseTag(tag)
		if err != nil {
			panic(err)
		}

		// Expect pointers to be foreignkeys and foreignkeys to have the form Field[Id]
		fieldName := refl.Name
		if kind == reflect.Ptr {
			fieldName += "Id"
		}

		// Transform struct keys to DB column names if needed
		var tableField string
		if g.admin.NameTransform != nil {
			tableField = g.admin.NameTransform(fieldName)
		} else {
			tableField = refl.Name
		}

		// Choose Field
		var field Field
		fmt.Println(kind)
		if fieldType, ok := tagMap["Field"]; ok {
			switch fieldType {
			case "url":
				field = &URLField{BaseField: &BaseField{}}
			default:
				field = &TextField{BaseField: &BaseField{}}
			}
		} else {
			switch kind {
			case reflect.String:
				field = &TextField{BaseField: &BaseField{}}
			case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				field = &IntField{BaseField: &BaseField{}}
			case reflect.Float32, reflect.Float64:
				field = &FloatField{BaseField: &BaseField{}}
			case reflect.Struct:
				field = &TimeField{BaseField: &BaseField{}}
			default:
				fmt.Println("NOOO")
				field = &TextField{BaseField: &BaseField{}}
			}
		}
		field.Attrs().name = fieldName

		// Read relevant config options from the tagMap
		err = field.Configure(tagMap)
		if err != nil {
			panic(err)
		}

		if label, ok := tagMap["label"]; ok {
			field.Attrs().label = label
		} else {
			field.Attrs().label = fieldName
		}

		field.Attrs().columnName = tableField

		if _, ok := tagMap["list"]; ok {
			field.Attrs().list = true
		}

		am.fields = append(am.fields, field)
	}

	g.admin.models[am.Slug] = &am
	g.Models = append(g.Models, &am)
	return nil
}

type model struct {
	Name      string
	Slug      string
	fields    []Field
	tableName string
	instance  interface{}
}

func (m *model) renderForm(w io.Writer, data []interface{}, errors []string) {
	hasData := len(data) == len(m.fieldNames())
	var val interface{}
	for i, fieldName := range m.fieldNames() {
		if hasData {
			val = data[i]
		}
		var err string
		if errors != nil {
			err = errors[i]
		}
		field := m.fieldByName(fieldName)
		field.Render(w, val, err)
	}
}

func (m *model) fieldNames() []string {
	names := []string{}
	for _, field := range m.fields {
		names = append(names, field.Attrs().name)
	}
	return names
}

func (m *model) tableColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		names = append(names, field.Attrs().columnName)
	}
	return names
}

func (m *model) listColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		if !field.Attrs().list {
			continue
		}
		names = append(names, field.Attrs().label)
	}
	return names
}

func (m *model) listTableColumns() []string {
	names := []string{}
	for _, field := range m.fields {
		if !field.Attrs().list {
			continue
		}
		names = append(names, field.Attrs().columnName)
	}
	return names
}

func (m *model) fieldByName(name string) Field {
	for _, field := range m.fields {
		if field.Attrs().name == name {
			return field
		}
	}
	return nil
}

func (a *Admin) modelURL(slug, action string) string {
	if _, ok := a.models[slug]; !ok {
		return a.Path
	}

	return fmt.Sprintf("%v/model/%v%v", a.Path, slug, action)
}
