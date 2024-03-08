/***************************************************************
 *
 * Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package registry

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite" // It doesn't require CGO
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/pkg/errors"
	"github.com/pressly/goose/v3"
	log "github.com/sirupsen/logrus"
	gormlog "github.com/thomas-tacquet/gormv2-logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/utils"
)

type RegistrationStatus string

// The AdminMetadata is used in [Namespace] as a marshaled JSON string
// to be stored in registry DB.
//
// The *UserID are meant to correspond to the "sub" claim of the user token that
// the OAuth client issues if the user is logged in using OAuth, or it should be
// "admin" from local password-based authentication.
//
// To prevent users from writing to certain fields (readonly), you may use "post" tag
// with value "exclude". This will exclude the field from user's create/update requests
// and the field will also be excluded from field discovery endpoint (OPTION method).
//
// We use validator package to validate struct fields from user requests. If a field is
// required, add `validate:"required"` to that field. This tag will also be used by fields discovery
// endpoint to tell the UI if a field is required. For other validator tags,
// visit: https://pkg.go.dev/github.com/go-playground/validator/v10
type AdminMetadata struct {
	UserID                string             `json:"user_id" post:"exclude"` // "sub" claim of user JWT who requested registration
	Description           string             `json:"description"`
	SiteName              string             `json:"site_name"`
	Institution           string             `json:"institution" validate:"required"` // the unique identifier of the institution
	SecurityContactUserID string             `json:"security_contact_user_id"`        // "sub" claim of user who is responsible for taking security concern
	Status                RegistrationStatus `json:"status" post:"exclude"`
	ApproverID            string             `json:"approver_id" post:"exclude"` // "sub" claim of user JWT who approved registration
	ApprovedAt            time.Time          `json:"approved_at" post:"exclude"`
	CreatedAt             time.Time          `json:"created_at" post:"exclude"`
	UpdatedAt             time.Time          `json:"updated_at" post:"exclude"`
}

type Namespace struct {
	ID            int                    `json:"id" post:"exclude" gorm:"primaryKey"`
	Prefix        string                 `json:"prefix" validate:"required"`
	Pubkey        string                 `json:"pubkey" validate:"required"`
	Identity      string                 `json:"identity" post:"exclude"`
	AdminMetadata AdminMetadata          `json:"admin_metadata" gorm:"serializer:json"`
	CustomFields  map[string]interface{} `json:"custom_fields" gorm:"serializer:json"`
}

type NamespaceWOPubkey struct {
	ID            int           `json:"id"`
	Prefix        string        `json:"prefix"`
	Pubkey        string        `json:"-"` // Don't include pubkey in this case
	Identity      string        `json:"identity"`
	AdminMetadata AdminMetadata `json:"admin_metadata"`
}

type Topology struct {
	ID     int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Prefix string `json:"prefix" gorm:"unique;not null"`
}

type ServerType string

const (
	OriginType ServerType = "origin"
	CacheType  ServerType = "cache"
)

const (
	Pending  RegistrationStatus = "Pending"
	Approved RegistrationStatus = "Approved"
	Denied   RegistrationStatus = "Denied"
	Unknown  RegistrationStatus = "Unknown"
)

/*
Declare the DB handle as an unexported global so that all
functions in the package can access it without having to
pass it around. This simplifies the HTTP handlers, and
the handle is already thread-safe! The approach being used
is based off of 1.b from
https://www.alexedwards.net/blog/organising-database-access
*/
var db *gorm.DB

//go:embed migrations/*.sql
var embedMigrations embed.FS

func (st ServerType) String() string {
	return string(st)
}

func (rs RegistrationStatus) String() string {
	return string(rs)
}

func (rs RegistrationStatus) LowerString() string {
	return strings.ToLower(string(rs))
}

func (a AdminMetadata) Equal(b AdminMetadata) bool {
	return a.UserID == b.UserID &&
		a.Description == b.Description &&
		a.SiteName == b.SiteName &&
		a.Institution == b.Institution &&
		a.SecurityContactUserID == b.SecurityContactUserID &&
		a.Status == b.Status &&
		a.ApproverID == b.ApproverID &&
		a.ApprovedAt.Equal(b.ApprovedAt) &&
		a.CreatedAt.Equal(b.CreatedAt) &&
		a.UpdatedAt.Equal(b.UpdatedAt)
}

func (Namespace) TableName() string {
	return "namespace"
}

func (Topology) TableName() string {
	return "topology"
}

func IsValidRegStatus(s string) bool {
	return s == "Pending" || s == "Approved" || s == "Denied" || s == "Unknown"
}

func createTopologyTable() error {
	err := db.AutoMigrate(&Topology{})
	if err != nil {
		return fmt.Errorf("Failed to migrate topology table: %v", err)
	}
	return nil
}

// Check if a namespace exists in either Topology or Pelican registry
func namespaceExistsByPrefix(prefix string) (bool, error) {
	var count int64

	err := db.Model(&Namespace{}).Where("prefix = ?", prefix).Count(&count).Error
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	} else {
		if config.GetPreferredPrefix() == "OSDF" {
			// Perform a count across both 'namespace' and 'topology' tables
			err := db.Model(&Topology{}).Where("prefix = ?", prefix).Count(&count).Error
			if err != nil {
				return false, err
			}
			return count > 0, nil
		}
	}
	return false, nil
}

func namespaceSupSubChecks(prefix string) (superspaces []string, subspaces []string, inTopo bool, err error) {
	// The very first thing we do is check if there's a match in topo -- if there is, for now
	// we simply refuse to allow registration of a superspace or a subspace, assuming the registrant
	// has to go through topology
	if config.GetPreferredPrefix() == "OSDF" {
		topoSuperSubQuery := `
		SELECT prefix FROM topology WHERE (? || '/') LIKE (prefix || '/%')
		UNION
		SELECT prefix FROM topology WHERE (prefix || '/') LIKE (? || '/%')
		`
		var results []Topology
		err = db.Raw(topoSuperSubQuery, prefix, prefix).Scan(&results).Error
		if err != nil {
			return
		}

		if len(results) > 0 {
			// If we get here, there was a match -- it's a trap!
			inTopo = true
			return
		}
	}

	// Check if any registered namespaces already superspace the incoming namespace,
	// eg if /foo is already registered, this will be true for an incoming /foo/bar because
	// /foo is logically above /foo/bar (according to my logic, anyway)
	superspaceQuery := `SELECT prefix FROM namespace WHERE (? || '/') LIKE (prefix || '/%')`
	err = db.Raw(superspaceQuery, prefix).Scan(&superspaces).Error
	if err != nil {
		return
	}

	// Check if any registered namespaces already subspace the incoming namespace,
	// eg if /foo/bar is already registered, this will be true for an incoming /foo because
	// /foo/bar is logically below /foo
	subspaceQuery := `SELECT prefix FROM namespace WHERE (prefix || '/') LIKE (? || '/%')`
	err = db.Raw(subspaceQuery, prefix).Scan(&subspaces).Error
	if err != nil {
		return
	}

	return
}

func namespaceExistsById(id int) (bool, error) {
	var namespaces []Namespace
	result := db.Limit(1).Find(&namespaces, id)
	if result.Error != nil {
		return false, result.Error
	} else {
		return result.RowsAffected > 0, nil
	}
}

func namespaceBelongsToUserId(id int, userId string) (bool, error) {
	var result Namespace
	err := db.First(&result, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, fmt.Errorf("Namespace with id = %d does not exists", id)
	} else if err != nil {
		return false, errors.Wrap(err, "error retrieving namespace")
	}
	return result.AdminMetadata.UserID == userId, nil
}

func getNamespaceJwksById(id int) (jwk.Set, error) {
	var result Namespace
	err := db.Select("pubkey").Where("id = ?", id).Last(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("namespace with id %d not found in database", id)
	} else if err != nil {
		return nil, errors.Wrap(err, "error retrieving pubkey")
	}

	set, err := jwk.ParseString(result.Pubkey)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse pubkey as a jwks")
	}

	return set, nil
}

func getNamespaceJwksByPrefix(prefix string) (jwk.Set, *AdminMetadata, error) {
	var result Namespace
	err := db.Select("pubkey", "admin_metadata").Where("prefix = ?", prefix).Last(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, fmt.Errorf("namespace with prefix %q not found in database", prefix)
	} else if err != nil {
		return nil, nil, errors.Wrap(err, "error retrieving pubkey")
	}

	set, err := jwk.ParseString(result.Pubkey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to parse pubkey as a jwks")
	}

	return set, &result.AdminMetadata, nil
}

func getNamespaceStatusById(id int) (RegistrationStatus, error) {
	if id < 1 {
		return "", errors.New("Invalid id. id must be a positive integer")
	}
	var result Namespace
	query := db.Select("admin_metadata").Where("id = ?", id).Last(&result)
	err := query.Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Unknown, fmt.Errorf("namespace with id %d not found in database", id)
	} else if err != nil {
		return Unknown, errors.Wrap(err, "error retrieving pubkey")
	}
	if result.AdminMetadata.Status == "" {
		return Unknown, nil
	}
	return result.AdminMetadata.Status, nil
}

func getNamespaceById(id int) (*Namespace, error) {
	if id < 1 {
		return nil, errors.New("Invalid id. id must be a positive number")
	}
	ns := Namespace{}
	err := db.Last(&ns, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("namespace with id %d not found in database", id)
	} else if err != nil {
		return nil, errors.Wrap(err, "error retrieving pubkey")
	}

	// By default, JSON unmarshal will convert any generic number to float
	// and we only allow integer in custom fields, so we convert them back
	for key, val := range ns.CustomFields {
		switch v := val.(type) {
		case float64:
			ns.CustomFields[key] = int(v)
		case float32:
			ns.CustomFields[key] = int(v)
		}
	}
	return &ns, nil
}

func getNamespaceByPrefix(prefix string) (*Namespace, error) {
	if prefix == "" {
		return nil, errors.New("Invalid prefix. Prefix must not be empty")
	}
	ns := Namespace{}
	err := db.Where("prefix = ? ", prefix).Last(&ns).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("namespace with id %q not found in database", prefix)
	} else if err != nil {
		return nil, errors.Wrap(err, "error retrieving pubkey")
	}

	// By default, JSON unmarshal will convert any generic number to float
	// and we only allow integer in custom fields, so we convert them back
	for key, val := range ns.CustomFields {
		switch v := val.(type) {
		case float64:
			ns.CustomFields[key] = int(v)
		case float32:
			ns.CustomFields[key] = int(v)
		}
	}
	return &ns, nil
}

// Get a collection of namespaces by filtering against various non-default namespace fields
// excluding Namespace.ID, Namespace.Identity, Namespace.Pubkey, and various dates
//
// For filterNs.AdminMetadata.Description and filterNs.AdminMetadata.SiteName,
// the string will be matched using `strings.Contains`. This is too mimic a SQL style `like` match.
// The rest of the AdminMetadata fields is matched by `==`
func getNamespacesByFilter(filterNs Namespace, serverType ServerType) ([]*Namespace, error) {
	query := `SELECT id, prefix, pubkey, identity, admin_metadata FROM namespace WHERE 1=1 `
	if serverType == CacheType {
		// Refer to the cache prefix name in cmd/cache_serve
		query += ` AND prefix LIKE '/caches/%'`
	} else if serverType == OriginType {
		query += ` AND NOT prefix LIKE '/caches/%'`
	} else if serverType != "" {
		return nil, errors.New(fmt.Sprint("Can't get namespace: unsupported server type: ", serverType))
	}
	if filterNs.CustomFields != nil {
		return nil, errors.New("Unsupported operation: Can't filter against Custrom Registration field.")
	}
	if filterNs.ID != 0 {
		return nil, errors.New("Unsupported operation: Can't filter against ID field.")
	}
	if filterNs.Identity != "" {
		return nil, errors.New("Unsupported operation: Can't filter against Identity field.")
	}
	if filterNs.Pubkey != "" {
		return nil, errors.New("Unsupported operation: Can't filter against Pubkey field.")
	}
	if filterNs.Prefix != "" {
		query += fmt.Sprintf(" AND prefix like '%%%s%%' ", filterNs.Prefix)
	}
	if !filterNs.AdminMetadata.ApprovedAt.Equal(time.Time{}) || !filterNs.AdminMetadata.UpdatedAt.Equal(time.Time{}) || !filterNs.AdminMetadata.CreatedAt.Equal(time.Time{}) {
		return nil, errors.New("Unsupported operation: Can't filter against date.")
	}
	// Always sort by id by default
	query += " ORDER BY id ASC"

	namespacesIn := []Namespace{}
	if err := db.Raw(query).Scan(&namespacesIn).Error; err != nil {
		return nil, err
	}

	namespacesOut := []*Namespace{}
	for idx, ns := range namespacesIn {
		if filterNs.AdminMetadata.UserID != "" && filterNs.AdminMetadata.UserID != ns.AdminMetadata.UserID {
			continue
		}
		if filterNs.AdminMetadata.Description != "" && !strings.Contains(ns.AdminMetadata.Description, filterNs.AdminMetadata.Description) {
			continue
		}
		if filterNs.AdminMetadata.SiteName != "" && !strings.Contains(ns.AdminMetadata.SiteName, filterNs.AdminMetadata.SiteName) {
			continue
		}
		if filterNs.AdminMetadata.Institution != "" && filterNs.AdminMetadata.Institution != ns.AdminMetadata.Institution {
			continue
		}
		if filterNs.AdminMetadata.SecurityContactUserID != "" && filterNs.AdminMetadata.SecurityContactUserID != ns.AdminMetadata.SecurityContactUserID {
			continue
		}
		if filterNs.AdminMetadata.Status != "" {
			if filterNs.AdminMetadata.Status == Unknown {
				if ns.AdminMetadata.Status != "" && ns.AdminMetadata.Status != Unknown {
					continue
				}
			} else if filterNs.AdminMetadata.Status != ns.AdminMetadata.Status {
				continue
			}
		}
		if filterNs.AdminMetadata.ApproverID != "" && filterNs.AdminMetadata.ApproverID != ns.AdminMetadata.ApproverID {
			continue
		}
		// Congrats! You passed all the filter check and this namespace matches what you want
		namespacesOut = append(namespacesOut, &namespacesIn[idx])
	}
	return namespacesOut, nil
}

func AddNamespace(ns *Namespace) error {
	// Adding default values to the field. Note that you need to pass other fields
	// including user_id before this function
	ns.AdminMetadata.CreatedAt = time.Now()
	ns.AdminMetadata.UpdatedAt = time.Now()
	// We only set status to pending when it's empty to allow unit tests to add a namespace with
	// desired status
	if ns.AdminMetadata.Status == "" {
		ns.AdminMetadata.Status = Pending
	}

	return db.Save(&ns).Error
}

func updateNamespace(ns *Namespace) error {
	existingNs, err := getNamespaceById(ns.ID)
	if err != nil || existingNs == nil {
		return errors.Wrap(err, "Failed to get namespace")
	}
	if ns.Prefix == "" {
		ns.Prefix = existingNs.Prefix
	}
	if ns.Pubkey == "" {
		ns.Pubkey = existingNs.Pubkey
	}
	// We intentionally exclude updating "identity" as this should only be updated
	// when user registered through Pelican client with identity
	ns.Identity = existingNs.Identity

	existingNsAdmin := existingNs.AdminMetadata
	// We prevent the following fields from being modified by the user for now.
	// They are meant for "internal" use only and we don't support changing
	// UserID on the fly. We also don't allow changing Status other than explicitly
	// call updateNamespaceStatusById
	ns.AdminMetadata.UserID = existingNsAdmin.UserID
	ns.AdminMetadata.CreatedAt = existingNsAdmin.CreatedAt
	ns.AdminMetadata.Status = existingNsAdmin.Status
	ns.AdminMetadata.ApprovedAt = existingNsAdmin.ApprovedAt
	ns.AdminMetadata.ApproverID = existingNsAdmin.ApproverID
	ns.AdminMetadata.UpdatedAt = time.Now()

	return db.Save(ns).Error
}

func updateNamespaceStatusById(id int, status RegistrationStatus, approverId string) error {
	ns, err := getNamespaceById(id)
	if err != nil {
		return errors.Wrap(err, "Error getting namespace by id")
	}

	ns.AdminMetadata.Status = status
	ns.AdminMetadata.UpdatedAt = time.Now()
	if status == Approved {
		if approverId == "" {
			return errors.New("approverId can't be empty to approve")
		}
		ns.AdminMetadata.ApproverID = approverId
		ns.AdminMetadata.ApprovedAt = time.Now()
	}

	adminMetadataByte, err := json.Marshal(ns.AdminMetadata)
	if err != nil {
		return errors.Wrap(err, "Error marshaling admin metadata")
	}

	return db.Model(ns).Where("id = ?", id).Update("admin_metadata", string(adminMetadataByte)).Error
}

func deleteNamespace(prefix string) error {
	// GORM by default uses transaction for write operations
	return db.Where("prefix = ?", prefix).Delete(&Namespace{}).Error
}

func getAllNamespaces() ([]*Namespace, error) {
	var namespaces []*Namespace
	if result := db.Order("id ASC").Find(&namespaces); result.Error != nil {
		return nil, result.Error
	}

	for _, ns := range namespaces {
		for key, val := range ns.CustomFields {
			switch v := val.(type) {
			case float64:
				ns.CustomFields[key] = int(v)
			case float32:
				ns.CustomFields[key] = int(v)
			}
		}
	}

	return namespaces, nil
}

// Update database schema based on migration files under /migrations folder
func MigrateDB(sqldb *sql.DB) error {
	goose.SetBaseFS(embedMigrations)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}

	if err := goose.Up(sqldb, "migrations"); err != nil {
		return err
	}
	return nil
}

func InitializeDB(ctx context.Context) error {
	dbPath := param.Registry_DbLocation.GetString()
	if dbPath == "" {
		err := errors.New("Could not get path for the namespace registry database.")
		log.Fatal(err)
		return err
	}

	// Before attempting to create the database, the path
	// must exist or sql.Open will panic.
	err := os.MkdirAll(filepath.Dir(dbPath), 0755)
	if err != nil {
		return errors.Wrap(err, "Failed to create directory for namespace registry database")
	}

	if len(filepath.Ext(dbPath)) == 0 { // No fp extension, let's add .sqlite so it's obvious what the file is
		dbPath += ".sqlite"
	}

	dbName := dbPath + "?_busy_timeout=5000&_journal_mode=WAL"

	globalLogLevel := log.GetLevel()
	var ormLevel logger.LogLevel
	if globalLogLevel == log.DebugLevel || globalLogLevel == log.TraceLevel || globalLogLevel == log.InfoLevel {
		ormLevel = logger.Info
	} else if globalLogLevel == log.WarnLevel {
		ormLevel = logger.Warn
	} else if globalLogLevel == log.ErrorLevel {
		ormLevel = logger.Error
	} else {
		ormLevel = logger.Info
	}

	gormLogger := gormlog.NewGormlog(
		gormlog.WithLogrusEntry(log.WithField("component", "gorm")),
		gormlog.WithGormOptions(gormlog.GormOptions{
			LogLatency: true,
			LogLevel:   ormLevel,
		}),
	)

	log.Debugln("Opening connection to sqlite DB", dbName)

	db, err = gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: gormLogger})

	if err != nil {
		return errors.Wrapf(err, "Failed to open the database with path: %s", dbPath)
	}

	sqldb, err := db.DB()

	if err != nil {
		return errors.Wrapf(err, "Failed to get sql.DB from gorm DB: %s", dbPath)
	}

	// Run database migrations
	if err := MigrateDB(sqldb); err != nil {
		return err
	}

	return nil
}

// Create a table in the registry to store namespace prefixes from topology
func PopulateTopology() error {
	// The topology table may already exist from before, it may not. Because of this
	// we need to add to the table any prefixes that are in topology, delete from the
	// table any that aren't in topology, and skip any that exist in both.

	// First get all that are in the table. At time of writing, this is ~57 entries,
	// and that number should be monotonically decreasing. We're safe to load into mem.
	var topologies []Topology
	if err := db.Model(&Topology{}).Select("prefix").Find(&topologies).Error; err != nil {
		return err
	}

	nsFromTopoTable := make(map[string]bool)
	for _, topo := range topologies {
		nsFromTopoTable[topo.Prefix] = true
	}

	// Next, get the values from topology
	namespaces, err := utils.GetTopologyJSON()
	if err != nil {
		return errors.Wrapf(err, "Failed to get topology JSON")
	}

	// Be careful here, the ns object we iterate over is from topology,
	// and it's not the same ns object we use elsewhere in this file.
	nsFromTopoJSON := make(map[string]bool)
	for _, ns := range namespaces.Namespaces {
		nsFromTopoJSON[ns.Path] = true
	}

	toAdd := []string{}
	toDelete := []string{}
	// If in topo and not in the table, add
	for prefix := range nsFromTopoJSON {
		if found := nsFromTopoTable[prefix]; !found {
			toAdd = append(toAdd, prefix)
		}
	}
	// If in table and not in topo, delete
	for prefix := range nsFromTopoTable {
		if found := nsFromTopoJSON[prefix]; !found {
			toDelete = append(toDelete, prefix)
		}
	}

	var toAddTopo []Topology
	for _, prefix := range toAdd {
		toAddTopo = append(toAddTopo, Topology{Prefix: prefix})
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("prefix IN ?", toDelete).Delete(&Topology{}).Error; err != nil {
			return err
		}

		if len(toAddTopo) > 0 {
			if err := tx.Create(&toAddTopo).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func PeriodicTopologyReload() {
	for {
		time.Sleep(param.Federation_TopologyReloadInterval.GetDuration())
		err := PopulateTopology()
		if err != nil {
			log.Warningf("Failed to re-populate topology table: %s. Will try again later",
				err)
		}
	}
}

func ShutdownDB() error {
	sqldb, err := db.DB()
	if err != nil {
		log.Errorln("Failure when getting database instance from gorm:", err)
		return err
	}
	err = sqldb.Close()
	if err != nil {
		log.Errorln("Failure when shutting down the database:", err)
	}
	return err
}
