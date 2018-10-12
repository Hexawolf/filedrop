package filedrop

import (
	"database/sql"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/satori/go.uuid"
)

var ErrFileDoesntExists = errors.New("file doesn't exists")

// filedrop server structure, implements http.Handler.
type Server struct {
	DB *db
	Conf Config
	Logger *log.Logger
}

func New(conf Config) (*Server, error) {
	s := new(Server)
	var err error

	s.Conf = conf
	s.Logger = log.New(os.Stderr, "filedrop ", log.LstdFlags)
	s.DB, err = openDB(conf.DB.Driver, conf.DB.DSN)
	return s, err
}

// AddFile adds file to storage and returns assigned UUID which can be directly
// substituted into URL.
func (s *Server) AddFile(contents io.Reader, maxUses uint, storeUntil time.Time) (string, error) {
	fileUUID := uuid.NewV4()
	outLocation := filepath.Join(s.Conf.StorageDir, fileUUID.String())

	_, err := os.Stat(outLocation)
	if err == nil {
		return "", errors.New("UUID collision detected")
	}

	file, err := os.Create(outLocation)
	if err != nil {
		return "", errors.Wrap(err, "file open")
	}
	if _, err := io.Copy(file, contents); err != nil {
		return "", errors.Wrap(err, "file write")
	}
	if err := s.DB.AddFile(nil, fileUUID.String(), maxUses, storeUntil); err != nil {
		os.Remove(outLocation)
		return "", errors.Wrap(err, "db add")
	}

	return fileUUID.String(), nil
}

// RemoveFile removes file from database and underlying storage.
func (s *Server) RemoveFile(fileUUID string) error {
	return s.removeFile(nil, fileUUID)
}

func (s *Server) removeFile(tx *sql.Tx, fileUUID string) error {
	fileLocation := filepath.Join(s.Conf.StorageDir, fileUUID)

	// Just to check validity.
	_, err := uuid.FromString(fileUUID)
	if err != nil {
		return errors.Wrap(err, "uuid parse")
	}

	if err := s.DB.RemoveFile(tx, fileUUID); err !=nil {
		return errors.Wrap(err, "db remove")
	}

	if err := os.Remove(fileLocation); err != nil {
		// TODO: Recover DB entry?
		return errors.Wrap(err, "file remove")
	}
	return nil
}

func (s *Server) OpenFile(fileUUID string) (io.Reader, error) {
	// Just to check validity.
	_, err := uuid.FromString(fileUUID)
	if err != nil {
		return nil, errors.Wrap(err, "uuid parse")
	}

	fileLocation := filepath.Join(s.Conf.StorageDir, fileUUID)
	file, err := os.Open(fileLocation)
	if err != nil {
		if os.IsNotExist(err) {
			s.removeFile(nil, fileUUID)
			return nil, ErrFileDoesntExists
		}
		return nil, err
	}
	return file, nil
}

// GetFile opens file for reading.
//
// Note that access using this function is equivalent to access
// through HTTP API, so it will count against usage count, for example.
// To avoid this use OpenFile(fileUUID).
func (s *Server) GetFile(fileUUID string) (io.Reader, error) {
	// Just to check validity.
	_, err := uuid.FromString(fileUUID)
	if err != nil {
		return nil, errors.Wrap(err, "uuid parse")
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, errors.Wrap(err, "tx begin")
	}
	defer tx.Rollback() // rollback is no-op after commit

	if s.DB.ShouldDelete(tx, fileUUID) {
		if err := s.removeFile(tx, fileUUID); err != nil {
			log.Println("Error while trying to remove file", fileUUID + ":", err)

		}
		if err := tx.Commit(); err != nil {
			log.Println("Tx commit error:", err)
			return nil, err
		}
		return nil, ErrFileDoesntExists
	}
	if err := s.DB.AddUse(tx, fileUUID); err != nil {
		return nil, errors.Wrap(err, "add use")
	}

	fileLocation := filepath.Join(s.Conf.StorageDir, fileUUID)
	file, err := os.Open(fileLocation)
	if err != nil {
		if os.IsNotExist(err) {
			s.removeFile(tx, fileUUID)
			return nil, ErrFileDoesntExists
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		log.Println("Tx commit error:", err)
		return nil, errors.Wrap(err, "tx commit")
	}
	return file, nil
}

func (s *Server) acceptFile(w http.ResponseWriter, r *http.Request) {
	splittenPath := strings.Split(r.URL.Path, "/")
	filename := splittenPath[len(splittenPath)-1]

	if s.Conf.UploadAuth.Callback != nil && !s.Conf.UploadAuth.Callback(r) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("403 forbidden"))
		return
	}

	if s.Conf.Limits.MaxFileSize != 0 && r.ContentLength > int64(s.Conf.Limits.MaxFileSize) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte("413 request entity too large"))
		return
	}

	storeUntil := time.Time{}
	if r.URL.Query().Get("store-secs") == "" && s.Conf.Limits.MaxStoreSecs != 0 {
		storeUntil = time.Now().Add(time.Duration(s.Conf.Limits.MaxStoreSecs) * time.Second)
	} else if r.URL.Query().Get("store-secs") != "" {
		secs, err := strconv.Atoi(r.URL.Query().Get("store-secs"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("400 bad request (invalid store-secs value)"))
			return
		}
		if uint(secs) > s.Conf.Limits.MaxStoreSecs {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("400 bad request (too big store-secs value)"))
			return
		}
		storeUntil = time.Now().Add(time.Duration(secs) * time.Second)
	}
	var maxUses uint
	if r.URL.Query().Get("max-uses") == "" && s.Conf.Limits.MaxUses != 0 {
		maxUses = s.Conf.Limits.MaxUses
	} else if r.URL.Query().Get("max-uses") != "" {
		var err error
		maxUses, err := strconv.Atoi(r.URL.Query().Get("max-uses"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("400 bad request (invalid max-uses value)"))
			return
		}
		if uint(maxUses) > s.Conf.Limits.MaxUses {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("400 bad request (too big max-uses value)"))
			return
		}
	}

	fileUUID, err := s.AddFile(r.Body, maxUses, storeUntil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	// Smart logic to convert request's URL into absolute result URL.
	resURL := url.URL{}
	if s.Conf.HTTPSUpstream {
		resURL.Scheme = "https"
	} else {
		resURL.Scheme = "http"
	}
	resURL.Host = r.Host
	// Drop last two components of path.
	splittenPath = splittenPath[:len(splittenPath)-1]
	splittenPath = append(splittenPath, fileUUID)
	splittenPath = append(splittenPath, filename)
	resURL.Path = strings.Join(splittenPath, "/")

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(resURL.String()))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if s.Conf.DownloadAuth.Callback != nil && !s.Conf.DownloadAuth.Callback(r) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("403 forbidden"))
		return
	}

	splittenPath := strings.Split(r.URL.Path, "/")
	if len(splittenPath) < 2 {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("404 not found"))
		return
	}
	fileUUID := splittenPath[len(splittenPath)-2]
	reader, err := s.GetFile(fileUUID)
	if err != nil {
		if err == ErrFileDoesntExists {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("404 not found"))

		} else {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
		}
		return
	}
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, reader)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.acceptFile(w, r)
	} else if r.Method == http.MethodGet {
		s.serveFile(w, r)
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("405 method not allowed"))
	}
}

func (s *Server) Close() error {
	return s.DB.Close()
}
