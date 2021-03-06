// The MIT License (MIT)

// Copyright (c) 2017 Fabian Wenzelmann

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package goauth

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultTimeFromScanType is the default function to return database entries
// to a time.Time.
func DefaultTimeFromScanType(val interface{}) (time.Time, error) {
	// first check if we already got a time.Time because parseTime in
	// the MySQL driver is true
	if alreadyTime, ok := val.(time.Time); ok {
		return alreadyTime, nil
	}
	if bytes, ok := val.([]byte); ok {
		s := string(bytes)
		// let's hope this is correct... however who came up with THIS parse
		// function definition in Go?!
		return time.Parse("2006-01-02 15:04:05", s)
	} else {
		// we have to return some time... why not now.
		return time.Now().UTC(), errors.New("Invalid date in database, probably a bug if you end up here.")
	}
}

// SQLSessionTemplate to generate queries for different SQL flavours such as MySQL
// or postgres. It must use certain placeholders for example for the table name or
// key length. See the MySQL implementation, it would be really cumbersome to
// document all the details.
type SQLSessionTemplate interface {
	// InitQ returns a query to initialize the session database.
	InitQ() string

	// GetQ is a query to select the user identifiaction, the time the key was
	// created and the time the until the key is valid from the database given
	// the session key.
	GetQ() string

	// CreateQ inserts the user identifiaction, the key, the time the key was
	// created and the time until the key is valid (in that order) in the database.
	CreateQ() string

	// DeleteForUserQ deletes all entries for a given user identifiaction from the
	// database.
	DeleteForUserQ() string

	// DeleteInvalidQ must delete all invalid keys from the database.
	DeleteInvalidQ() string

	// DeleteKeyQ deletes the entry for a given key from the database.
	DeleteKeyQ() string

	// TimeFromScanType is a rather odd function, but time fields are handled
	// differently in different handlers and some handlers even have options to change
	// that behaviour. Therefor when we get a time field (via the Row.Scan or some
	// other scan function) we pass a pointer to an interface{} to it.
	// Whatever that type is depends on the handler.
	// Example: The MySQL driver by default gets the datetime field as string and has
	// an option to parse it as time.Time (though this is disabled by default).
	// So the MySQL implementation first checks if val is already of time.Time and
	// then returns this value. If it is not why try to read it as a string and parse
	// this string to a time.Time. Postgres uses time.Time already.
	TimeFromScanType(val interface{}) (time.Time, error)
}

// SQLSessionHandler is an implementation of SessionHandler that uses a predinfed
// set of SQL queries. These queries are generated in NewSQLSessionHandler and stored
// in strings here. The reason we do that is that SQLSessionTemplate uses
// placeholders and we want to avoid calling fmt.Sprintf on every query. So now
// given all the details we simply generate the queries from the template once in
// NewSQLSessionHandler and replace the table name and key size once.
// The handler also requires the sql.DB database.
type SQLSessionHandler struct {
	// DB is the database to operate on.
	DB *sql.DB

	// The queries required by this handler.
	InitQ, GetQ, CreateQ, DeleteForUserQ, DeleteInvalidQ, DeleteKeyQ string

	// TableName is the name of the session table, by default user_sessions.
	TableName string

	// UserIDType is the sql type that is used to store the user identifiaction.
	UserIDType string

	// KeySize is the length of the key strings.
	KeySize int

	// TimeFromScanType: See TimeFromScanType in the documentation of SQLSessionTemplate.
	TimeFromScanType func(val interface{}) (time.Time, error)

	// ForceUIDuint forces the user id to be of type uint64.
	// This field exists because most drivers stoer big ints simply as int, which
	// would mean we could never have more than 2^32 users. I Mean must people don't
	// have that but I thought it just to be thorough to enforce unsinged ints.
	ForceUIDuint bool
	// this is required for example for sqlite, it does not support
	// multiple goroutines when writing!
	// I hope this does not slow us down too much...
	mutex   sync.RWMutex
	blockDB bool
}

// NewSQLSessionHandler compiles the query template with the given information.
// tableName is the name of the SQL table, if set to "" it defaults to
// "user_sessions".
// userIDType is the SQL user identifiaction type, if set to "" it defaults to
// "BIGINT UNSIGNED NOT NULL".
//
// The lockDB argument is used for sqlite3 (and maybe other drivers):
// sqlite3 does not support writing from multiple goroutines and thus the database
// has to be locked. If set to true a mutex will be used to synchronize access to
// the database.
//
// See documentation of SQLSessionHandler for more details.
func NewSQLSessionHandler(db *sql.DB, t SQLSessionTemplate, tableName, userIDType string, lockDB bool) *SQLSessionHandler {
	if tableName == "" {
		tableName = "user_sessions"
	}
	if userIDType == "" {
		userIDType = "BIGINT UNSIGNED NOT NULL"
	}
	// I'm not so happy with this many lines of code, but I don't want to use
	// the reflect package or something either...
	h := SQLSessionHandler{DB: db, TableName: tableName,
		UserIDType: userIDType, KeySize: DefaultKeyLength,
		TimeFromScanType: t.TimeFromScanType, ForceUIDuint: false, blockDB: lockDB}
	h.InitQ = fmt.Sprintf(t.InitQ(), h.TableName, h.UserIDType, h.KeySize)
	h.GetQ = fmt.Sprintf(t.GetQ(), h.TableName)
	h.CreateQ = fmt.Sprintf(t.CreateQ(), h.TableName)
	h.DeleteForUserQ = fmt.Sprintf(t.DeleteForUserQ(), h.TableName)
	h.DeleteInvalidQ = fmt.Sprintf(t.DeleteInvalidQ(), h.TableName)
	h.DeleteKeyQ = fmt.Sprintf(t.DeleteKeyQ(), h.TableName)
	return &h
}

func (c *SQLSessionHandler) Init() error {
	if c.blockDB {
		c.mutex.Lock()
		defer c.mutex.Unlock()
	}
	_, err := c.DB.Exec(c.InitQ)
	return err
}

func (c *SQLSessionHandler) GetData(key string) (*SessionKeyData, error) {
	if c.blockDB {
		c.mutex.RLock()
		defer c.mutex.RUnlock()
	}
	var uid, createdVal, validUntilVal interface{}
	var err error
	row := c.DB.QueryRow(c.GetQ, key)
	if c.ForceUIDuint {
		var uidUint uint64
		err = row.Scan(&uidUint, &createdVal, &validUntilVal)
		uid = uidUint
	} else {
		err = row.Scan(&uid, &createdVal, &validUntilVal)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	created, err := c.TimeFromScanType(createdVal)
	if err != nil {
		return nil, err
	}
	validUntil, err := c.TimeFromScanType(validUntilVal)
	if err != nil {
		return nil, err
	}
	// everything ok
	val := SessionKeyData{User: uid, CreationTime: created, ValidUntil: validUntil}
	return &val, nil
}

func (c *SQLSessionHandler) CreateEntry(user UserKeyType, key string, validDuration time.Duration) (*SessionKeyData, error) {
	if c.blockDB {
		c.mutex.Lock()
		defer c.mutex.Unlock()
	}
	data := CurrentTimeKeyData(user, validDuration)
	_, err := c.DB.Exec(c.CreateQ, user, key, data.CreationTime, data.ValidUntil)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *SQLSessionHandler) DeleteEntriesForUser(user UserKeyType) (int64, error) {
	if c.blockDB {
		c.mutex.Lock()
		defer c.mutex.Unlock()
	}
	res, err := c.DB.Exec(c.DeleteForUserQ, user)
	if err != nil {
		return -1, err
	}
	num, err := res.RowsAffected()
	if err != nil {
		return -1, nil
	}
	return num, nil
}

func (c *SQLSessionHandler) DeleteInvalidKeys() (int64, error) {
	now := CurrentTime()
	if c.blockDB {
		c.mutex.Lock()
		defer c.mutex.Unlock()
	}
	res, err := c.DB.Exec(c.DeleteInvalidQ, now)
	if err != nil {
		return -1, err
	}
	num, err := res.RowsAffected()
	if err != nil {
		return -1, nil
	}
	return num, nil
}

func (c *SQLSessionHandler) DeleteKey(key string) error {
	if c.blockDB {
		c.mutex.Lock()
		defer c.mutex.Unlock()
	}
	_, err := c.DB.Exec(c.DeleteKeyQ, key)
	return err
}

// MySQLSessionTemplate implements SQLSessionTemplate with MySQL queries.
type MySQLSessionTemplate struct {
}

// NewMySQLSessionTemplate returns a new MySQLSessionTemplate.
func NewMySQLSessionTemplate() MySQLSessionTemplate {
	return MySQLSessionTemplate{}
}

// NewMYSQLSessionHandler returns a new SQLSessionHandler that uses MySQL.
func NewMySQLSessionHandler(db *sql.DB, tableName, userIDType string) *SQLSessionHandler {
	return NewSQLSessionHandler(db, NewMySQLSessionTemplate(), tableName, userIDType, false)
}

// NewMySQLSessionController returns a new SessionController that uses a MySQL
// database.
func NewMySQLSessionController(db *sql.DB, tableName, userIDType string) *SessionController {
	handler := NewMySQLSessionHandler(db, tableName, userIDType)
	return NewSessionController(handler)
}

func (t MySQLSessionTemplate) InitQ() string {
	return `CREATE TABLE IF NOT EXISTS %s (
		user_id %s,
		session_key CHAR(%d) NOT NULL,
    created DATETIME NOT NULL,
    valid_until DATETIME NOT NULL,
		PRIMARY KEY (session_key)
	);`
}

func (t MySQLSessionTemplate) GetQ() string {
	return "SELECT user_id, created, valid_until FROM %s WHERE session_key = ?;"
}

func (t MySQLSessionTemplate) CreateQ() string {
	return "INSERT INTO %s (user_id, session_key, created, valid_until) VALUES (?, ?, ?, ?);"
}

func (t MySQLSessionTemplate) DeleteForUserQ() string {
	return "DELETE FROM %s WHERE user_id = ?;"
}

func (t MySQLSessionTemplate) DeleteInvalidQ() string {
	return "DELETE FROM %s WHERE ? > valid_until;"
}

func (t MySQLSessionTemplate) DeleteKeyQ() string {
	return "DELETE FROM %s WHERE session_key = ?"
}

// TimeFromScanType for MySQL first checks if the value is already a time.Time
// (the driver has an option to enable this).
// If not it pasres the datetime in the format "2006-01-02 15:04:05".
func (t MySQLSessionTemplate) TimeFromScanType(val interface{}) (time.Time, error) {
	return DefaultTimeFromScanType(val)
}

// SQLite3SessionTemplate is an implementation of SQLSessionTemplate
// using sqlite3 queries.
// Nearly all MySQL queries work, so we simply delegate it to a MySQLSessionTemplate
// and implement the different queries again.
type SQLite3SessionTemplate struct {
	// most of the stuff is the same as for SQL, so we can actually simply
	// delegate it to this template and just define the new queries
	MySQLSessionTemplate
}

// NewSQLite3SessionTemplate returns a new SQLite3SessionTemplate.
func NewSQLite3SessionTemplate() *SQLite3SessionTemplate {
	return &SQLite3SessionTemplate{MySQLSessionTemplate: NewMySQLSessionTemplate()}
}

func (*SQLite3SessionTemplate) InitQ() string {
	return `CREATE TABLE IF NOT EXISTS %s (
		user_id %s,
		session_key CHAR(%d) NOT NULL PRIMARY KEY,
    created DATETIME NOT NULL,
    valid_until DATETIME NOT NULL
	);`
}

// NewSQLite3SessionHandler returns a new SQLSessionHandler that uses
// sqlite3.
func NewSQLite3SessionHandler(db *sql.DB, tableName, userIDType string) *SQLSessionHandler {
	return NewSQLSessionHandler(db, NewSQLite3SessionTemplate(), tableName, userIDType, true)
}

// NewSQLite3SessionController returns a SessionController that uses sqlite3.
func NewSQLite3SessionController(db *sql.DB, tableName, userIDType string) *SessionController {
	handler := NewSQLite3SessionHandler(db, tableName, userIDType)
	return NewSessionController(handler)
}

// PostgresSessionTemplate ist an implementation of SQLSessionTemplate for psotgres.
type PostgresSessionTemplate struct{}

// NewPostgresSessionTemplate returns a new PostgresSessionTemplate.
func NewPostgresSessionTemplate() PostgresSessionTemplate {
	return PostgresSessionTemplate{}
}

func (t PostgresSessionTemplate) InitQ() string {
	return `CREATE TABLE IF NOT EXISTS %s (
		user_id %s,
		session_key CHAR(%d) NOT NULL,
    created TIMESTAMP NOT NULL,
    valid_until TIMESTAMP NOT NULL,
		PRIMARY KEY (session_key)
	);`
}

func (t PostgresSessionTemplate) GetQ() string {
	return "SELECT user_id, created, valid_until FROM %s WHERE session_key = $1;"
}

func (t PostgresSessionTemplate) CreateQ() string {
	return "INSERT INTO %s (user_id, session_key, created, valid_until) VALUES ($1, $2, $3, $4);"
}

func (t PostgresSessionTemplate) DeleteForUserQ() string {
	return "DELETE FROM %s WHERE user_id = $1;"
}

func (t PostgresSessionTemplate) DeleteInvalidQ() string {
	return "DELETE FROM %s WHERE $1 > valid_until;"
}

func (t PostgresSessionTemplate) DeleteKeyQ() string {
	return "DELETE FROM %s WHERE session_key = $1"
}

func (t PostgresSessionTemplate) TimeFromScanType(val interface{}) (time.Time, error) {
	return DefaultTimeFromScanType(val)
}

// NewPostgresSessionHandler returns a new SQLSessionHandler using postgres.
// It changes the default value of userIDType (the NewSQLSessionHandler uses
// BIGINT UNSIGNED NOT NULL). In postgres there is no unsigned keyword, so we use
// "BIGINT NOT NULL" as default.
func NewPostgresSessionHandler(db *sql.DB, tableName, userIDType string) *SQLSessionHandler {
	if userIDType == "" {
		userIDType = "BIGINT NOT NULL"
	}
	return NewSQLSessionHandler(db, NewPostgresSessionTemplate(), tableName, userIDType, false)
}

// NewPostgresSessionController returns a new SessionController using postgres.
// It changes the default value of userIDType (the NewSQLSessionHandler uses
// BIGINT UNSIGNED NOT NULL). In postgres there is no unsigned keyword, so we use
// "BIGINT NOT NULL" as default.
func NewPostgresSessionController(db *sql.DB, tableName, userIDType string) *SessionController {
	handler := NewPostgresSessionHandler(db, tableName, userIDType)
	return NewSessionController(handler)
}

// USERS stuff

// SQLUserQueries stores several queries for working
// with users on SQL databases.
// These queries can be different for different SQL flavours
// and thus there different methods that create such
// an object, for example MySQLUserQueries.
// In general there is one database for all user information
// called "users".
// The default scheme that should be implemented looks as
// follows (in MySQL syntax):
//
//   CREATE TABLE IF NOT EXISTS users (
// 		id SERIAL,
// 		username VARCHAR(150) NOT NULL,
// 		first_name VARCHAR(30) NOT NULL,
// 		last_name VARCHAR(30) NOT NULL,
// 		email VARCHAR(254),
// 		password CHAR(<PWLENGTH>),
// 		is_active BOOL,
// 		last_login DATETIME,
// 		PRIMARY KEY(id),
// 		UNIQUE(username)
// 	);
//
// On the wiki there are more notes on how to alter this
// scheme: https://github.com/FabianWe/goauth/wiki/Manage-Users#the-default-user-scheme
type SQLUserQueries struct {
	// PwLength is the length of the database hashes stored in
	// the database, needed to initialize the database with the
	// correct length.
	PwLength int

	// InitQuery is the query to generate the "users" table.
	// It should take care take when executing this command
	// no error is returned if the table already exists.
	// It should take care to adjust the length of the password
	// field to the PwLength.
	InitQuery string

	// InsertQuery is a query to insert a user to the default
	// scheme.
	// It must use placeholders (? in MySQL, $i in postgre)
	// for the information that will be stored.
	// The values are passed in the following order:
	// username, first_name, last_name, email, password, is_active, last_login
	// username, first_name, last_name, email are of type string,
	// password is of type []byte, is_active of type bool
	// and last_login of type time.Time.
	InsertQuery string

	// ValidateQuery must be a query that selects exactly
	// two values: the id and the password column given
	// the username.
	// You must use one placeholder that gets replaced by the
	// username. Example in MySQL:
	// "SELECT id, password FROM users WHERE username = ?"
	ValidateQuery string

	// UpdatePasswordQuery is the query to update the password for a given username.
	UpdatePasswordQuery string

	// ListUsersQuery is the query to select all available users.
	//
	// New in version v0.4
	ListUsersQuery string

	// GetUsernameQ is the query used to get the user name given an id.
	//
	// New in version v0.4
	GetUsernameQ string

	// DeleteUserQ is used to delete the user give the user name.
	//
	// New in version v0.4
	DeleteUserQ string

	// GetUserInfoQuery is the query to get the information for a given username
	// from the default scheme.
	//
	// New in version v0.5
	GetUserInfoQuery string

	// GetIDQuery is the query used to get the id given a username.
	//
	// New in version v0.6
	GetIDQuery string

	// TimeFromScanType is used to transform database time entries to
	// gos time. See SQLSessionHandler for details.
	// Defaults to a function that first checks if the value is already a time.Time
	// and otherwise parses the value as a string in NewSQLUserQueries.
	//
	// New in version v0.5
	TimeFromScanType func(val interface{}) (time.Time, error)
}

// MySQLUserQueries provides queries to use with MySQL.
func MySQLUserQueries(pwLength int) *SQLUserQueries {
	initQ := `
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL,
		username VARCHAR(150) NOT NULL,
		first_name VARCHAR(30) NOT NULL,
		last_name VARCHAR(30) NOT NULL,
		email VARCHAR(254),
		password CHAR(%d),
		is_active BOOL,
		last_login DATETIME,
		PRIMARY KEY(id),
		UNIQUE(username)
	);
	`
	initQ = fmt.Sprintf(initQ, pwLength)
	insertQ := `
	INSERT INTO users (username, first_name, last_name, email, password, is_active, last_login)
		VALUES(?, ?, ?, ?, ?, ?, ?);
	`
	validateQ := "SELECT id, password FROM users WHERE username = ?"
	updateQ := "UPDATE users SET password=? WHERE username=?"
	listUsersQ := "SELECT id, username FROM users"
	getUsernameQ := "SELECT username FROM users WHERE id=?"
	deleteQ := "DELETE FROM users WHERE username=?"
	getUserInfoQ := "SELECT id, first_name, last_name, email, is_active, last_login FROM users WHERE username=?"
	getIDQuery := "SELECT id FROM users WHERE username=?"
	return &SQLUserQueries{PwLength: pwLength, InitQuery: initQ,
		InsertQuery: insertQ, ValidateQuery: validateQ, UpdatePasswordQuery: updateQ,
		ListUsersQuery: listUsersQ, GetUsernameQ: getUsernameQ,
		DeleteUserQ: deleteQ, GetUserInfoQuery: getUserInfoQ,
		GetIDQuery: getIDQuery, TimeFromScanType: DefaultTimeFromScanType}
}

// PostgresUserQueries provides queries to use with postgres.
func PostgresUserQueries(pwLength int) *SQLUserQueries {
	initQ := `
	CREATE TABLE IF NOT EXISTS users (
		id bigserial,
		username varchar(150) NOT NULL,
		first_name varchar(30) NOT NULL,
		last_name varchar(30) NOT NULL,
		email varchar(254),
		password char(%d),
		is_active bool NOT NULL,
		last_login timestamp NOT NULL,
		unique (username)
	);
	`
	initQ = fmt.Sprintf(initQ, pwLength)
	insertQ := `
	INSERT INTO users (username, first_name, last_name, email, password, is_active, last_login)
		VALUES ($1, $2, $3, $4, $5, $6, $7);
	`
	validateQ := "SELECT id, password FROM users WHERE username = $1"
	updateQ := "UPDATE users SET password=$1 WHERE username = $2"
	listUsersQ := "SELECT id, username FROM users"
	getUsernameQ := "SELECT username FROM users WHERE id = $1"
	deleteQ := "DELETE FROM users WHERE username = $1"
	getUserInfoQ := "SELECT id, first_name, last_name, email, is_active, last_login FROM users WHERE username = $1"
	getIDQuery := "SELECT id FROM users WHERE username = $1"
	return &SQLUserQueries{PwLength: pwLength, InitQuery: initQ,
		InsertQuery: insertQ, ValidateQuery: validateQ, UpdatePasswordQuery: updateQ,
		ListUsersQuery: listUsersQ, GetUsernameQ: getUsernameQ,
		DeleteUserQ: deleteQ, GetUserInfoQuery: getUserInfoQ,
		GetIDQuery: getIDQuery, TimeFromScanType: DefaultTimeFromScanType}
}

// SQLite3UserQueries provides queries to use with sqlite3.
func SQLite3UserQueries(pwLength int) *SQLUserQueries {
	// nearly everything is the same as for mysql
	res := MySQLUserQueries(pwLength)
	initQ := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY,
		username VARCHAR(150) NOT NULL,
		first_name VARCHAR(30) NOT NULL,
		last_name VARCHAR(30) NOT NULL,
		email VARCHAR(254),
		password CHAR(%d),
		is_active BOOL,
		last_login DATETIME,
		UNIQUE(username)
	);
	`
	initQ = fmt.Sprintf(initQ, pwLength)
	res.InitQuery = initQ
	return res
}

// SQLUserHandler implements the UserHandler by executing
// queries as defined in an instance of SQLUserQueries.
type SQLUserHandler struct {
	// SQLUserQueries are the queries used to access the database.
	*SQLUserQueries

	// DB is the database to execute the queries on.
	DB *sql.DB

	// PwHandler is used to encrypt / validate passwords.
	PwHandler PasswordHandler

	// required for example for sqlite
	blockDB bool
	mutex   sync.RWMutex
}

// NewSQLUserHandler returns a new SQLUserHandler given
// the queries and all the other information required.
// queries are the queries used to access the database.
//
// db is the database to execute the queries on.
//
// pwHandler is used to encrypt / validate passwords.
// Set this to nil if you want to use the default handler
// (bcrypt with cost 13).
//
// blockDB should be set to true if your database does not
// support access to the database by different goroutines.
// This is for example an issue with sqlite3.
// I'm not very happy to have it here since I think that's
// the job of the database driver, but we need it until
// there's a safe implementation of sqlite3.
// If it is set to true access to the database will be
// controlled with a mutex.
// For MySQL and postgres there is no need for this, the
// drivers handle this.
func NewSQLUserHandler(queries *SQLUserQueries, db *sql.DB, pwHandler PasswordHandler, blockDB bool) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = DefaultPWHandler
	}
	return &SQLUserHandler{SQLUserQueries: queries, DB: db, PwHandler: pwHandler, blockDB: blockDB}
}

// NewMySQLUserHandler returns a new handler that uses MySQL.
func NewMySQLUserHandler(db *sql.DB, pwHandler PasswordHandler) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = DefaultPWHandler
	}
	return NewSQLUserHandler(MySQLUserQueries(pwHandler.PasswordHashLength()),
		db, pwHandler, false)
}

// NewSQLite3UserHandler returns a new handler that uses
// sqlite3. Note that sqlite3 is really slow with this stuff!
func NewSQLite3UserHandler(db *sql.DB, pwHandler PasswordHandler) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = DefaultPWHandler
	}
	return NewSQLUserHandler(SQLite3UserQueries(pwHandler.PasswordHashLength()),
		db, pwHandler, true)
}

// NewPostgresUserHandler returns a new handler that uses
// postgres.
func NewPostgresUserHandler(db *sql.DB, pwHandler PasswordHandler) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = DefaultPWHandler
	}
	return NewSQLUserHandler(PostgresUserQueries(pwHandler.PasswordHashLength()),
		db, pwHandler, false)
}

func (handler *SQLUserHandler) Init() error {
	if handler.blockDB {
		handler.mutex.Lock()
		defer handler.mutex.Unlock()
	}
	_, err := handler.DB.Exec(handler.InitQuery)
	return err
}

func (handler *SQLUserHandler) Insert(userName, firstName, lastName, email string, plainPW []byte) (uint64, error) {
	now := CurrentTime()
	// try to encrypt the pw
	encrypted, encErr := handler.PwHandler.GenerateHash(plainPW)
	if encErr != nil {
		return NoUserID, encErr
	}

	if handler.blockDB {
		handler.mutex.Lock()
		defer handler.mutex.Unlock()
	}
	res, err := handler.DB.Exec(handler.InsertQuery, userName, firstName, lastName, email, encrypted, true, now)
	if err != nil {
		return NoUserID, err
	}

	// insert worked, try to get the last insert id
	insertInt, getErr := res.LastInsertId()
	if getErr != nil {
		return NoUserID, nil
	}
	// Don't know if this is even possible, but ok
	if insertInt < 0 {
		return NoUserID, nil
	}
	// everything ok, we convert to uint64
	var insertId uint64 = uint64(insertInt)
	return insertId, nil
}

func (handler *SQLUserHandler) Validate(userName string, cleartextPwCheck []byte) (uint64, error) {
	if handler.blockDB {
		handler.mutex.RLock()
		defer handler.mutex.RUnlock()
	}
	// first try to get the id and the password
	row := handler.DB.QueryRow(handler.ValidateQuery, userName)
	var userId uint64
	var hashPw []byte
	if err := row.Scan(&userId, &hashPw); err != nil {
		if err == sql.ErrNoRows {
			return NoUserID, ErrUserNotFound
		}
		return NoUserID, err
	}
	// validate the password
	test, err := handler.PwHandler.CheckPassword(hashPw, cleartextPwCheck)
	if err != nil {
		return NoUserID, err
	}
	// no error, check if passwords did match
	if test {
		return userId, nil
	} else {
		return NoUserID, nil
	}
}

func (handler *SQLUserHandler) UpdatePassword(username string, plainPW []byte) error {
	// try to encrypt the pw
	encrypted, encErr := handler.PwHandler.GenerateHash(plainPW)
	if encErr != nil {
		return encErr
	}

	if handler.blockDB {
		handler.mutex.Lock()
		defer handler.mutex.Unlock()
	}

	// now try to update the password
	_, err := handler.DB.Exec(handler.UpdatePasswordQuery, encrypted, username)
	return err
}

func (handler *SQLUserHandler) ListUsers() (map[uint64]string, error) {
	if handler.blockDB {
		handler.mutex.RLock()
		defer handler.mutex.RUnlock()
	}

	// try to get the results
	rows, err := handler.DB.Query(handler.ListUsersQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := make(map[uint64]string, 0)
	for rows.Next() {
		var id uint64
		var username string
		scanErr := rows.Scan(&id, &username)
		if scanErr != nil {
			return nil, scanErr
		}
		res[id] = username
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (handler *SQLUserHandler) GetUserName(id uint64) (string, error) {
	if handler.blockDB {
		handler.mutex.RLock()
		defer handler.mutex.RUnlock()
	}
	row := handler.DB.QueryRow(handler.GetUsernameQ, id)
	var username string
	if err := row.Scan(&username); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrUserNotFound
		}
		return "", err
	}
	return username, nil
}

func (handler *SQLUserHandler) DeleteUser(username string) error {
	if handler.blockDB {
		handler.mutex.Lock()
		defer handler.mutex.Unlock()
	}
	_, err := handler.DB.Exec(handler.DeleteUserQ, username)
	return err
}

func (handler *SQLUserHandler) GetUserID(userName string) (uint64, error) {
	if handler.blockDB {
		handler.mutex.RLock()
		defer handler.mutex.RUnlock()
	}
	row := handler.DB.QueryRow(handler.GetIDQuery, userName)
	var id uint64
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return NoUserID, ErrUserNotFound
		}
		return NoUserID, err
	}
	return id, nil
}

// getUserInfoQ := "SELECT id, first_name, last_name, email, is_active, last_login FROM users WHERE id=?"
func (handler *SQLUserHandler) GetUserBaseInfo(userName string) (*BaseUserInformation, error) {
	row := handler.DB.QueryRow(handler.GetUserInfoQuery, userName)
	var id uint64
	var firstName, lastName, email string
	var isActive bool
	var lastLoginVal interface{}
	if err := row.Scan(&id, &firstName, &lastName, &email, &isActive, &lastLoginVal); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	lastLogin, loginParseErr := handler.TimeFromScanType(lastLoginVal)
	if loginParseErr != nil {
		return nil, loginParseErr
	}
	res := &BaseUserInformation{ID: id, UserName: userName, FirstName: firstName,
		LastName: lastName, Email: email, LastLogin: lastLogin, IsActive: isActive}
	return res, nil
}
