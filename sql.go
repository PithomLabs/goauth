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

// General SQL implementation with interfaces, MySQL etc. below.
type SQLSessionTemplate interface {
	InitQ() string
	GetQ() string
	CreateQ() string
	DeleteForUserQ() string
	DeleteInvalidQ() string
	DeleteKeyQ() string
	TimeFromScanType(val interface{}) (time.Time, error)
}

type SQLSessionHandler struct {
	DB                                                               *sql.DB
	InitQ, GetQ, CreateQ, DeleteForUserQ, DeleteInvalidQ, DeleteKeyQ string
	TableName                                                        string
	UserIDType                                                       string
	KeySize                                                          int
	TimeFromScanType                                                 func(val interface{}) (time.Time, error)
	ForceUIDuint                                                     bool
	// this is required for example for sqlite, it does not support
	// multiple goroutines when writing!
	// I hope this does not slow us down too much...
	mutex   sync.RWMutex
	blockDB bool
}

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

func NewMySQLController(db *sql.DB, tableName, userIDType string) *SessionController {
	handler := NewSQLSessionHandler(db, NewMySQLSessionTemplate(), tableName, userIDType, false)
	return NewSessionController(handler)
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
			return nil, KeyNotFoundErr
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

type MySQLSessionTemplate struct {
}

func NewMySQLSessionTemplate() MySQLSessionTemplate {
	return MySQLSessionTemplate{}
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
	return "DELETE FROM %s WHERE valid_until > ?;"
}

func (t MySQLSessionTemplate) DeleteKeyQ() string {
	return "DELETE FROM %s WHERE session_key = ?"
}

func (t MySQLSessionTemplate) TimeFromScanType(val interface{}) (time.Time, error) {
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

type SQLite3SessionTemplate struct {
	// most of the stuff is the same as for SQL, so we can actually simply
	// delegate it to this template and just define the new queries
	MySQLSessionTemplate
}

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

func NewSQLite3SessionController(db *sql.DB, tableName, userIDType string) *SessionController {
	handler := NewSQLSessionHandler(db, NewSQLite3SessionTemplate(), tableName, userIDType, true)
	return NewSessionController(handler)
}

// USERS stuff

type SQLUserQueries struct {
	PwLength      int
	InitQuery     string
	InsertQuery   string
	ValidateQuery string
}

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
	return &SQLUserQueries{PwLength: pwLength, InitQuery: initQ,
		InsertQuery: insertQ, ValidateQuery: validateQ}
}

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

type SQLUserHandler struct {
	*SQLUserQueries
	DB        *sql.DB
	PwHandler PasswordHandler
	// required for example for sqlite
	blockDB bool
	mutex   sync.RWMutex
}

func NewSQLUserHandler(queries *SQLUserQueries, db *sql.DB, pwHandler PasswordHandler, blockDB bool) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = NewBcryptHandler(-1)
	}
	return &SQLUserHandler{SQLUserQueries: queries, DB: db, PwHandler: pwHandler, blockDB: blockDB}
}

func NewMySQLUserHandler(db *sql.DB, pwHandler PasswordHandler) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = NewBcryptHandler(-1)
	}
	return NewSQLUserHandler(MySQLUserQueries(pwHandler.PasswordHashLength()),
		db, pwHandler, false)
}

func NewSQLite3UserHandler(db *sql.DB, pwHandler PasswordHandler) *SQLUserHandler {
	if pwHandler == nil {
		pwHandler = NewBcryptHandler(-1)
	}
	return NewSQLUserHandler(SQLite3UserQueries(pwHandler.PasswordHashLength()),
		db, pwHandler, true)
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
