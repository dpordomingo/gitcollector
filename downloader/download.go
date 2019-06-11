package downloader

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/src-d/gitcollector/library"
	"github.com/src-d/go-borges"
	"github.com/src-d/go-borges/siva"
	"gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/util"
	"gopkg.in/src-d/go-errors.v1"
	"gopkg.in/src-d/go-git.v4"
)

var (
	// ErrNotDownloadJob is returned when a not download job is found.
	ErrNotDownloadJob = errors.NewKind("not download job")

	// ErrRepoAlreadyExists is returned if there is an attempt to
	// retrieve an already downloaded git repository.
	ErrRepoAlreadyExists = errors.NewKind("%s already downloaded")

	// ErrNeedSivaLibrary is returned when a siva borges.Library is needed.
	ErrNeedSivaLibrary = errors.NewKind("a siva library is needed")
)

// Download is a library.JobFn function to download a git repository and store
// it in a borges.Library.
func Download(_ context.Context, job *library.Job) error {
	if len(job.Endpoints) == 0 || job.Lib == nil || job.TempFS == nil {
		return ErrNotDownloadJob.New()
	}

	lib, ok := (job.Lib).(*siva.Library)
	if !ok {
		return ErrNeedSivaLibrary.New()
	}

	return downloadRepo(lib, job.TempFS, job.Endpoints[0])
}

func downloadRepo(
	lib *siva.Library,
	tmp billy.Filesystem,
	endpoint string,
) error {
	var (
		id     = buildRepoID(endpoint)
		repoID = borges.RepositoryID(id)
	)

	ok, _, _, err := lib.Has(repoID)
	if err != nil {
		return err
	}

	if ok {
		return ErrRepoAlreadyExists.New(id)
	}

	clonePath := filepath.Join(
		cloneRootPath,
		fmt.Sprintf("%s_%d", id, time.Now().UnixNano()),
	)

	repo, err := cloneRepo(tmp, clonePath, endpoint, id)
	if err != nil {
		return err
	}

	defer func() {
		if err := util.RemoveAll(tmp, clonePath); err != nil {
			// TODO: log errors
		}
	}()

	commit, err := headCommit(repo, id)
	if err != nil {
		return err
	}

	root, err := rootCommit(repo, commit)
	if err != nil {
		return err
	}

	var (
		locID = borges.LocationID(root.Hash.String())
		r     borges.Repository
	)

	loc, err := lib.Location(locID)
	if err != nil && !borges.ErrLocationNotExists.Is(err) {
		return err
	}

	if borges.ErrLocationNotExists.Is(err) {
		loc, err = lib.AddLocation(locID)
		if err == nil {
			r, err = createRootedRepo(
				loc,
				repoID,
				tmp,
				clonePath,
			)
		}
	}

	if err != nil {
		return err
	}

	if r == nil {
		r, err = loc.Get(repoID, borges.RWMode)
		if err != nil {
			r, err = loc.Init(repoID)
			if err != nil {
				return err
			}
		}
	}

	if _, err := createRemote(r.R(), id, endpoint); err != nil {
		return err
	}

	if err := r.R().Fetch(&git.FetchOptions{
		RemoteName: id,
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}

	return r.Commit()
}

func buildRepoID(endpoint string) string {
	endpoint = strings.TrimSuffix(endpoint, ".git")
	endpoint = strings.TrimPrefix(endpoint, "git://")
	endpoint = strings.TrimPrefix(endpoint, "git@")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.Replace(endpoint, ":", "/", 1)
	return endpoint
}

func createRootedRepo(
	loc borges.Location,
	repoID borges.RepositoryID,
	clonedFS billy.Filesystem,
	clonedPath string,
) (borges.Repository, error) {
	repo, err := loc.Init(repoID)
	if err != nil {
		return nil, err
	}

	if err := recursiveCopy(
		"/", repo.FS(),
		clonedPath, clonedFS,
	); err != nil {
		return nil, err
	}

	return repo, nil
}

func recursiveCopy(
	dst string,
	dstFS billy.Filesystem,
	src string,
	srcFS billy.Filesystem,
) error {
	stat, err := srcFS.Stat(src)
	if err != nil {
		return err
	}

	if stat.IsDir() {
		err = dstFS.MkdirAll(dst, stat.Mode())
		if err != nil {
			return err
		}

		files, err := srcFS.ReadDir(src)
		if err != nil {
			return err
		}

		for _, file := range files {
			srcPath := filepath.Join(src, file.Name())
			dstPath := filepath.Join(dst, file.Name())

			err = recursiveCopy(dstPath, dstFS, srcPath, srcFS)
			if err != nil {
				return err
			}
		}
	} else {
		err = copyFile(dst, dstFS, src, srcFS, stat.Mode())
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(
	dst string,
	dstFS billy.Filesystem,
	src string,
	srcFS billy.Filesystem,
	mode os.FileMode,
) error {
	_, err := srcFS.Stat(src)
	if err != nil {
		return err
	}

	fo, err := srcFS.Open(src)
	if err != nil {
		return err
	}
	defer fo.Close()

	fd, err := dstFS.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer fd.Close()

	_, err = io.Copy(fd, fo)
	if err != nil {
		fd.Close()
		dstFS.Remove(dst)
		return err
	}

	return nil
}

func addRemote(
	loc borges.Location,
	id, endpoint string,
) (borges.Repository, error) {
	repo, err := loc.Get(borges.RepositoryID(id), borges.RWMode)
	if err == nil {
		return repo, nil
	}

	repo, err = loc.Init(borges.RepositoryID(id))
	if err != nil {
		return nil, err
	}

	r := repo.R()
	_, err = createRemote(r, id, endpoint)
	if err != nil {
		return nil, err
	}

	return repo, nil
}