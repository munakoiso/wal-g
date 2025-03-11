package s3

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/pkg/errors"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

const (
	NotFoundAWSErrorCode  = "NotFound"
	NoSuchKeyAWSErrorCode = "NoSuchKey"

	VersioningDefault  = ""
	VersioningEnabled  = "enabled"
	VersioningDisabled = "disabled"
)

// TODO: Unit tests
type Folder struct {
	s3API    s3iface.S3API
	uploader *Uploader
	bucket   *string
	path     string
	config   *Config
}

func NewFolder(
	s3API s3iface.S3API,
	uploader *Uploader,
	path string,
	config *Config,
) *Folder {
	// Trim leading slash because there's no difference between absolute and relative paths in S3.
	path = strings.TrimPrefix(path, "/")
	return &Folder{
		uploader: uploader,
		s3API:    s3API,
		bucket:   aws.String(config.Bucket),
		path:     storage.AddDelimiterToPath(path),
		config:   config,
	}
}

func (folder *Folder) Exists(objectRelativePath string) (bool, error) {
	objectPath := folder.path + objectRelativePath
	stopSentinelObjectInput := &s3.HeadObjectInput{
		Bucket: folder.bucket,
		Key:    aws.String(objectPath),
	}

	_, err := folder.s3API.HeadObject(stopSentinelObjectInput)
	if err != nil {
		if isAwsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrapf(err, "failed to check s3 object '%s' existence", objectPath)
	}
	return true, nil
}

func (folder *Folder) PutObject(name string, content io.Reader) error {
	return folder.uploader.upload(context.Background(), *folder.bucket, folder.path+name, content) //TODO
}

func (folder *Folder) PutObjectWithContext(ctx context.Context, name string, content io.Reader) error {
	return folder.uploader.upload(ctx, *folder.bucket, folder.path+name, content) //TODO
}

func (folder *Folder) CopyObject(srcPath string, dstPath string) error {
	if exists, err := folder.Exists(srcPath); !exists {
		if err == nil {
			return storage.NewObjectNotFoundError(srcPath)
		}
		return err
	}
	source := path.Join(*folder.bucket, folder.path, srcPath)
	dst := path.Join(folder.path, dstPath)
	input := &s3.CopyObjectInput{CopySource: &source, Bucket: folder.bucket, Key: &dst}
	_, err := folder.s3API.CopyObject(input)
	return err
}

func (folder *Folder) ReadObject(objectRelativePath string) (io.ReadCloser, error) {
	objectPath := folder.path + objectRelativePath
	input := &s3.GetObjectInput{
		Bucket: folder.bucket,
		Key:    aws.String(objectPath),
	}

	object, err := folder.s3API.GetObject(input)
	if err != nil {
		if isAwsNotExist(err) {
			return nil, storage.NewObjectNotFoundError(objectPath)
		}
		return nil, errors.Wrapf(err, "failed to read object: '%s' from S3", objectPath)
	}

	reader := object.Body
	if folder.config.RangeBatchEnabled {
		reader = NewRangeReader(object.Body, objectPath, folder.config.RangeMaxRetries, folder)
	}
	return reader, nil
}

func (folder *Folder) GetSubFolder(subFolderRelativePath string) storage.Folder {
	subFolder := NewFolder(
		folder.s3API,
		folder.uploader,
		storage.JoinPath(folder.path, subFolderRelativePath)+"/",
		folder.config,
	)
	return subFolder
}

func (folder *Folder) GetPath() string {
	return folder.path
}

func (folder *Folder) ListFolder() (objects []storage.Object, subFolders []storage.Folder, err error) {
	prefix := aws.String(folder.path)
	delimiter := aws.String("/")

	if folder.isVersioningEnabled() {
		objects, subFolders, err = folder.listVersions(prefix, delimiter)
		if err != nil {
			return nil, nil, err
		}
	} else {
		listFunc := func(commonPrefixes []*s3.CommonPrefix, contents []*s3.Object) {
			for _, prefix := range commonPrefixes {
				subFolder := NewFolder(folder.s3API, folder.uploader, *prefix.Prefix, folder.config)
				subFolders = append(subFolders, subFolder)
			}
			for _, object := range contents {
				// Some storages return root tar_partitions folder as a Key.
				// We do not want to fail restoration due to this fact.
				// Keep in mind that skipping files is very dangerous and any decision here must be weighted.
				if *object.Key == folder.path {
					continue
				}

				objectRelativePath := strings.TrimPrefix(*object.Key, folder.path)
				objects = append(objects, storage.NewLocalObject(objectRelativePath, *object.LastModified, *object.Size))
			}
		}

		err = folder.listObjectsPages(prefix, delimiter, nil, listFunc)

		// DigitalOcean Spaces compatibility: DO's API complains about NoSuchKey when trying to list folders
		// which don't yet exist.
		if err != nil && !isAwsNotExist(err) {
			return nil, nil, errors.Wrapf(err, "failed to list s3 folder: '%s'", folder.path)
		}
	}

	return objects, subFolders, nil
}

func (folder *Folder) listVersions(prefix *string, delimiter *string) ([]storage.Object, []storage.Folder, error) {
	objects := []storage.Object{}
	subFolders := []storage.Folder{}
	versionsListFunc := func(out *s3.ListObjectVersionsOutput, _ bool) bool {
		for _, prefix := range out.CommonPrefixes {
			subFolder := NewFolder(folder.s3API, folder.uploader, *prefix.Prefix, folder.config)
			subFolders = append(subFolders, subFolder)
		}

		for _, object := range out.Versions {
			// Some storages return root tar_partitions folder as a Key.
			if *object.Key == folder.path {
				continue
			}

			objectRelativePath := strings.TrimPrefix(*object.Key, folder.path)
			if *object.IsLatest {
				objects = append(objects, storage.NewLocalObjectWithAdditionalInfo(objectRelativePath, *object.LastModified,
					*object.Size, fmt.Sprintf("%s LATEST", *object.VersionId)))
			} else {
				objects = append(objects, storage.NewLocalObjectWithAdditionalInfo(objectRelativePath, *object.LastModified,
					*object.Size, *object.VersionId))
			}
		}
		for _, object := range out.DeleteMarkers {
			// Some storages return root tar_partitions folder as a Key.
			if *object.Key == folder.path {
				continue
			}

			objectRelativePath := strings.TrimPrefix(*object.Key, folder.path)
			if *object.IsLatest {
				objects = append(objects, storage.NewLocalObjectWithAdditionalInfo(objectRelativePath, *object.LastModified,
					0, fmt.Sprintf("%s LATEST", *object.VersionId)))
			} else {
				objects = append(objects, storage.NewLocalObjectWithAdditionalInfo(objectRelativePath, *object.LastModified, 0, *object.VersionId))
			}
		}
		return true
	}
	input := &s3.ListObjectVersionsInput{
		Bucket:    folder.bucket,
		Prefix:    prefix,
		Delimiter: delimiter,
	}
	err := folder.s3API.ListObjectVersionsPages(input, versionsListFunc)

	// DigitalOcean Spaces compatibility: DO's API complains about NoSuchKey when trying to list folders
	// which don't yet exist.
	if err != nil && !isAwsNotExist(err) {
		return nil, nil, errors.Wrapf(err, "failed to list s3 folder: '%s'", folder.path)
	}
	return objects, subFolders, nil
}

func (folder *Folder) listObjectsPages(prefix *string, delimiter *string, maxKeys *int64,
	listFunc func(commonPrefixes []*s3.CommonPrefix, contents []*s3.Object)) (err error) {
	if folder.config.UseListObjectsV1 {
		err = folder.listObjectsPagesV1(prefix, delimiter, maxKeys, listFunc)
	} else {
		err = folder.listObjectsPagesV2(prefix, delimiter, maxKeys, listFunc)
	}
	return
}

func (folder *Folder) listObjectsPagesV1(prefix *string, delimiter *string, maxKeys *int64,
	listFunc func(commonPrefixes []*s3.CommonPrefix, contents []*s3.Object)) error {
	s3Objects := &s3.ListObjectsInput{
		Bucket:    folder.bucket,
		Prefix:    prefix,
		Delimiter: delimiter,
		MaxKeys:   maxKeys,
	}

	err := folder.s3API.ListObjectsPages(s3Objects, func(files *s3.ListObjectsOutput, lastPage bool) bool {
		listFunc(files.CommonPrefixes, files.Contents)
		return true
	})
	return err
}

func (folder *Folder) listObjectsPagesV2(prefix *string, delimiter *string, maxKeys *int64,
	listFunc func(commonPrefixes []*s3.CommonPrefix, contents []*s3.Object)) error {
	s3Objects := &s3.ListObjectsV2Input{
		Bucket:    folder.bucket,
		Prefix:    prefix,
		Delimiter: delimiter,
		MaxKeys:   maxKeys,
	}
	err := folder.s3API.ListObjectsV2Pages(s3Objects, func(files *s3.ListObjectsV2Output, lastPage bool) bool {
		listFunc(files.CommonPrefixes, files.Contents)
		return true
	})
	return err
}

func (folder *Folder) DeleteObjects(objectRelativePaths []string) error {
	parts := partitionStrings(objectRelativePaths, 1000)
	needsVersioning := folder.isVersioningEnabled()

	for _, part := range parts {
		input := &s3.DeleteObjectsInput{Bucket: folder.bucket, Delete: &s3.Delete{
			Objects: folder.partitionToObjects(part, needsVersioning),
		}}
		_, err := folder.s3API.DeleteObjects(input)
		if err != nil {
			return errors.Wrapf(err, "failed to delete s3 object: '%s'", part)
		}
	}
	return nil
}

func (folder *Folder) getObjectVersions(key string) ([]*s3.ObjectIdentifier, error) {
	inp := &s3.ListObjectVersionsInput{
		Bucket: folder.bucket,
		Prefix: aws.String(folder.path + key),
	}

	out, err := folder.s3API.ListObjectVersions(inp)
	if err != nil {
		return nil, err
	}
	list := make([]*s3.ObjectIdentifier, 0)
	for _, version := range out.Versions {
		list = append(list, &s3.ObjectIdentifier{Key: version.Key, VersionId: version.VersionId})
	}

	for _, deleteMarker := range out.DeleteMarkers {
		list = append(list, &s3.ObjectIdentifier{Key: deleteMarker.Key, VersionId: deleteMarker.VersionId})
	}

	return list, nil
}

func (folder *Folder) isVersioningEnabled() bool {
	switch folder.config.EnableVersioning {
	case VersioningEnabled:
		return true
	case VersioningDisabled:
		return false
	case VersioningDefault:
		result, err := folder.s3API.GetBucketVersioning(&s3.GetBucketVersioningInput{
			Bucket: folder.bucket,
		})
		if err != nil {
			return false
		}

		if result.Status != nil && *result.Status == s3.BucketVersioningStatusEnabled {
			folder.config.EnableVersioning = VersioningEnabled
			return true
		}
		folder.config.EnableVersioning = VersioningDisabled
	}
	return false
}

func (folder *Folder) Validate() error {
	prefix := aws.String(folder.path)
	delimiter := aws.String("/")
	int64One := int64(1)
	input := &s3.ListObjectsInput{
		Bucket:    folder.bucket,
		Prefix:    prefix,
		Delimiter: delimiter,
		MaxKeys:   &int64One,
	}
	_, err := folder.s3API.ListObjects(input)
	if err != nil {
		return fmt.Errorf("bad credentials: %v", err)
	}
	return nil
}

func (folder *Folder) SetVersioningEnabled(enable bool) {
	if enable && folder.isVersioningEnabled() {
		folder.config.EnableVersioning = VersioningEnabled
	} else {
		folder.config.EnableVersioning = VersioningDisabled
	}
}

func (folder *Folder) partitionToObjects(keys []string, versioningEnabled bool) []*s3.ObjectIdentifier {
	objects := make([]*s3.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		if versioningEnabled {
			versions, err := folder.getObjectVersions(key)
			if err != nil {
				tracelog.ErrorLogger.Printf("failed to list versions: %v", err)
				//TODO to error or not to error
			}
			objects = append(objects, versions...)
		} else {
			objects = append(objects, &s3.ObjectIdentifier{Key: aws.String(folder.path + key)})
		}
		//objects[id] = &s3.ObjectIdentifier{Key: aws.String(folder.path + key)}
	}
	return objects
}

func isAwsNotExist(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		if awsErr.Code() == NotFoundAWSErrorCode || awsErr.Code() == NoSuchKeyAWSErrorCode {
			return true
		}
	}
	return false
}
