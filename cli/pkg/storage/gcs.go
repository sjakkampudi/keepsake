package storage

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/api/iterator"

	"replicate.ai/cli/pkg/console"
)

type GCSStorage struct {
	bucketName string
	client     *storage.Client
}

func NewGCSStorage(bucket string) (*GCSStorage, error) {
	client, err := storage.NewClient(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to Google Cloud Storage, got error: %s", err)
	}

	return &GCSStorage{
		bucketName: bucket,
		client:     client,
	}, nil
}

func (s *GCSStorage) RootURL() string {
	return "gs://" + s.bucketName
}

func (s *GCSStorage) Get(path string) ([]byte, error) {
	pathString := fmt.Sprintf("gs://%s/%s", s.bucketName, path)
	bucket := s.client.Bucket(s.bucketName)
	obj := bucket.Object(path)
	reader, err := obj.NewReader(context.TODO())
	if err != nil {
		if err == storage.ErrObjectNotExist {
			return nil, &NotExistError{msg: "Get: path does not exist: " + path}
		}
		return nil, fmt.Errorf("Failed to open %s, got error: %s", pathString, err)
	}
	defer reader.Close()
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("Failed to read %s, got error: %s", pathString, err)
	}

	return data, nil
}

// Delete deletes path. If path is a directory, it recursively deletes
// all everything under path
func (s *GCSStorage) Delete(path string) error {
	console.Debug("Deleting gs://%s/%s...", s.bucketName, path)
	err := s.applyRecursive(path, 128, func(obj *storage.ObjectHandle) error {
		return obj.Delete(context.TODO())
	})
	if err != nil {
		return fmt.Errorf("Failed to delete gs://%s/%s: %w", s.bucketName, path, err)
	}
	return nil
}

// Put data at path
func (s *GCSStorage) Put(path string, data []byte) error {
	// TODO
	return nil
}

func (s *GCSStorage) PutDirectory(localPath string, storagePath string) error {
	// TODO
	return nil
}

// List files in a path non-recursively
func (s *GCSStorage) List(dir string) ([]string, error) {
	results := []string{}

	// prefixes must end with / and must not end with /
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	dir = strings.TrimPrefix(dir, "/")

	bucket := s.client.Bucket(s.bucketName)
	it := bucket.Objects(context.TODO(), &storage.Query{
		Prefix:    dir,
		Delimiter: "/",
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Failed to list gs://%s/%s", s.bucketName, dir)
		}
		results = append(results, attrs.Name)
	}
	return results, nil
}

func (s *GCSStorage) MatchFilenamesRecursive(results chan<- ListResult, folder string, filename string) {
	s.listRecursive(results, folder, func(key string) bool {
		return filepath.Base(key) == filename
	})
}

func (s *GCSStorage) listRecursive(results chan<- ListResult, folder string, filter func(string) bool) {
	// prefixes must end with / and must not end with /
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	folder = strings.TrimPrefix(folder, "/")

	bucket := s.client.Bucket(s.bucketName)
	it := bucket.Objects(context.TODO(), &storage.Query{
		Prefix: folder,
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			close(results)
			break
		}
		if err != nil {
			results <- ListResult{Error: fmt.Errorf("Failed to list gs://%s/%s", s.bucketName, folder)}
		}
		if filter(attrs.Name) {
			results <- ListResult{Path: attrs.Name}
		}
	}
}

// GetDirectory recursively copies storageDir to localDir
func (s *GCSStorage) GetDirectory(storageDir string, localDir string) error {
	err := s.applyRecursive(storageDir, int64(128), func(obj *storage.ObjectHandle) error {
		gcsPathString := fmt.Sprintf("gs://%s/%s", s.bucketName, obj.ObjectName())
		reader, err := obj.NewReader(context.TODO())
		if err != nil {
			return fmt.Errorf("Failed to open %s, got error: %w", gcsPathString, err)
		}
		defer reader.Close()

		relPath, err := filepath.Rel(storageDir, obj.ObjectName())
		if err != nil {
			return fmt.Errorf("Failed to determine directory of %s relative to %s, got error: %w", obj.ObjectName(), storageDir, err)
		}
		localPath := filepath.Join(localDir, relPath)
		localDir := filepath.Dir(localPath)
		if err := os.MkdirAll(localDir, 0755); err != nil {
			return fmt.Errorf("Failed to create directory %s, got error: %w", localDir, err)
		}

		f, err := os.Create(localPath)
		if err != nil {
			return fmt.Errorf("Failed to create file %s, got error: %w", localPath, err)
		}
		defer f.Close()

		console.Debug("Downloading %s to %s", gcsPathString, localPath)
		if _, err := io.Copy(f, reader); err != nil {
			return fmt.Errorf("Failed to copy %s to %s, got error: %w", gcsPathString, localPath, err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("Failed to copy gs://%s/%s to %s, got error: %w", s.bucketName, storageDir, localDir, err)
	}
	return nil
}

func (s *GCSStorage) applyRecursive(dir string, concurrency int64, fn func(obj *storage.ObjectHandle) error) error {
	sem := semaphore.NewWeighted(concurrency)
	group, ctx := errgroup.WithContext(context.Background())
	bucket := s.client.Bucket(s.bucketName)
	it := bucket.Objects(context.TODO(), &storage.Query{
		Prefix: dir,
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}

		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		group.Go(func() error {
			defer sem.Release(1)
			obj := bucket.Object(attrs.Name)
			return fn(obj)
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	return nil
}
