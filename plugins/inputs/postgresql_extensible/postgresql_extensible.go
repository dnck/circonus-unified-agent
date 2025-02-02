package postgresqlextensible

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/circonus-labs/circonus-unified-agent/cua"
	"github.com/circonus-labs/circonus-unified-agent/internal"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs/postgresql"
	_ "github.com/jackc/pgx/stdlib" //nolint:golint
)

type Postgresql struct {
	postgresql.Service
	Databases      []string
	AdditionalTags []string
	Query          query
	Debug          bool

	Log cua.Logger
}

type query []struct {
	Sqlquery    string
	Script      string
	Version     int
	Withdbname  bool
	Tagvalue    string
	Measurement string
}

var ignoredColumns = map[string]bool{"stats_reset": true}

var sampleConfig = `
  instance_id = "" # unique instance identifier (REQUIRED)

  ## specify address via a url matching:
  ##   postgres://[pqgotest[:password]]@localhost[/dbname]\
  ##       ?sslmode=[disable|verify-ca|verify-full]
  ## or a simple string:
  ##   host=localhost user=pqgotest password=... sslmode=... dbname=app_production
  #
  ## All connection parameters are optional.  #
  ## Without the dbname parameter, the driver will default to a database
  ## with the same name as the user. This dbname is just for instantiating a
  ## connection with the server and doesn't restrict the databases we are trying
  ## to grab metrics for.
  #
  address = "host=localhost user=postgres sslmode=disable"

  ## connection configuration.
  ## maxlifetime - specify the maximum lifetime of a connection.
  ## default is forever (0s)
  max_lifetime = "0s"

  ## A list of databases to pull metrics about. If not specified, metrics for all
  ## databases are gathered.
  ## databases = ["app_production", "testing"]
  #
  ## A custom name for the database that will be used as the "server" tag in the
  ## measurement output. If not specified, a default one generated from
  ## the connection address is used.
  # outputaddress = "db01"
  #
  ## Define the toml config where the sql queries are stored
  ## New queries can be added, if the withdbname is set to true and there is no
  ## databases defined in the 'databases field', the sql query is ended by a
  ## 'is not null' in order to make the query succeed.
  ## Example :
  ## The sqlquery : "SELECT * FROM pg_stat_database where datname" become
  ## "SELECT * FROM pg_stat_database where datname IN ('postgres', 'pgbench')"
  ## because the databases variable was set to ['postgres', 'pgbench' ] and the
  ## withdbname was true. Be careful that if the withdbname is set to false you
  ## don't have to define the where clause (aka with the dbname) the tagvalue
  ## field is used to define custom tags (separated by commas)
  ## The optional "measurement" value can be used to override the default
  ## output measurement name ("postgresql").
  ##
  ## The script option can be used to specify the .sql file path.
  ## If script and sqlquery options specified at same time, sqlquery will be used 
  ##
  ## Structure :
  ## [[inputs.postgresql_extensible.query]]
  ##   sqlquery string
  ##   version string
  ##   withdbname boolean
  ##   tagvalue string (comma separated)
  ##   measurement string
  [[inputs.postgresql_extensible.query]]
    sqlquery="SELECT * FROM pg_stat_database"
    version=901
    withdbname=false
    tagvalue=""
    measurement=""
  [[inputs.postgresql_extensible.query]]
    sqlquery="SELECT * FROM pg_stat_bgwriter"
    version=901
    withdbname=false
    tagvalue="postgresql.stats"
`

func (p *Postgresql) Init() error {
	var err error
	for i := range p.Query {
		if p.Query[i].Sqlquery == "" {
			p.Query[i].Sqlquery, err = ReadQueryFromFile(p.Query[i].Script)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Postgresql) SampleConfig() string {
	return sampleConfig
}

func (p *Postgresql) Description() string {
	return "Read metrics from one or many postgresql servers"
}

func (p *Postgresql) IgnoredColumns() map[string]bool {
	return ignoredColumns
}

func ReadQueryFromFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open (%s): %w", filePath, err)
	}
	defer file.Close()

	query, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("readall (%s): %w", filePath, err)
	}
	return string(query), nil
}

func (p *Postgresql) Gather(ctx context.Context, acc cua.Accumulator) error {
	var (
		err        error
		sqlQuery   string
		queryAddon string
		dbVersion  int
		query      string
		tagValue   string
		measName   string
		columns    []string
	)

	// Retrieving the database version
	query = `SELECT setting::integer / 100 AS version FROM pg_settings WHERE name = 'server_version_num'`
	if err = p.DB.QueryRow(query).Scan(&dbVersion); err != nil {
		dbVersion = 0
	}

	// We loop in order to process each query
	// Query is not run if Database version does not match the query version.
	for i := range p.Query {
		sqlQuery = p.Query[i].Sqlquery
		tagValue = p.Query[i].Tagvalue

		if p.Query[i].Measurement != "" {
			measName = p.Query[i].Measurement
		} else {
			measName = "postgresql"
		}

		if p.Query[i].Withdbname {
			if len(p.Databases) != 0 {
				queryAddon = fmt.Sprintf(` IN ('%s')`,
					strings.Join(p.Databases, "','"))
			} else {
				queryAddon = " is not null"
			}
		} else {
			queryAddon = ""
		}
		sqlQuery += queryAddon

		if p.Query[i].Version <= dbVersion {
			rows, err := p.DB.Query(sqlQuery)
			if err != nil {
				p.Log.Error(err.Error())
				continue
			}

			defer rows.Close()

			// grab the column information from the result
			if columns, err = rows.Columns(); err != nil {
				p.Log.Error(err.Error())
				continue
			}

			p.AdditionalTags = nil
			if tagValue != "" {
				tagList := strings.Split(tagValue, ",")
				for t := range tagList {
					p.AdditionalTags = append(p.AdditionalTags, tagList[t])
				}
			}

			for rows.Next() {
				err = p.accRow(measName, rows, acc, columns)
				if err != nil {
					p.Log.Error(err.Error())
					break
				}
			}
		}
	}
	return nil
}

type scanner interface {
	Scan(dest ...interface{}) error
}

func (p *Postgresql) accRow(measName string, row scanner, acc cua.Accumulator, columns []string) error {
	var (
		err        error
		columnVars []interface{}
		dbname     bytes.Buffer
		tagAddress string
	)

	// this is where we'll store the column name with its *interface{}
	columnMap := make(map[string]*interface{})

	for _, column := range columns {
		columnMap[column] = new(interface{})
	}

	// populate the array of interface{} with the pointers in the right order
	for i := 0; i < len(columnMap); i++ {
		columnVars = append(columnVars, columnMap[columns[i]])
	}

	// deconstruct array of variables and send to Scan
	if err = row.Scan(columnVars...); err != nil {
		return fmt.Errorf("row scan: %w", err)
	}

	if c, ok := columnMap["datname"]; ok && *c != nil {
		// extract the database name from the column map
		switch datname := (*c).(type) {
		case string:
			dbname.WriteString(datname)
		default:
			dbname.WriteString("postgres")
		}
	} else {
		dbname.WriteString("postgres")
	}

	if tagAddress, err = p.SanitizedAddress(); err != nil {
		return fmt.Errorf("sanitize addr: %w", err)
	}

	// Process the additional tags
	tags := map[string]string{
		"server": tagAddress,
		"db":     dbname.String(),
	}

	fields := make(map[string]interface{})
COLUMN:
	for col, val := range columnMap {
		p.Log.Debugf("Column: %s = %T: %v\n", col, *val, *val)
		_, ignore := ignoredColumns[col]
		if ignore || *val == nil {
			continue
		}

		for _, tag := range p.AdditionalTags {
			if col != tag {
				continue
			}
			switch v := (*val).(type) {
			case string:
				tags[col] = v
			case []byte:
				tags[col] = string(v)
			case int64, int32, int:
				tags[col] = fmt.Sprintf("%d", v)
			default:
				p.Log.Debugf("Failed to add %q as additional tag", col)
			}
			continue COLUMN
		}

		if v, ok := (*val).([]byte); ok {
			fields[col] = string(v)
		} else {
			fields[col] = *val
		}
	}
	acc.AddFields(measName, fields, tags)
	return nil
}

func init() {
	inputs.Add("postgresql_extensible", func() cua.Input {
		return &Postgresql{
			Service: postgresql.Service{
				MaxIdle: 1,
				MaxOpen: 1,
				MaxLifetime: internal.Duration{
					Duration: 0,
				},
				IsPgBouncer: false,
			},
		}
	})
}
