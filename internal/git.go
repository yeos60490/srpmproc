package internal

import (
	"fmt"
	"git.rockylinux.org/release-engineering/public/srpmproc/internal/data"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type remoteTarget struct {
	remote string
	when   time.Time
}

type remoteTargetSlice []remoteTarget

func (p remoteTargetSlice) Len() int {
	return len(p)
}

func (p remoteTargetSlice) Less(i, j int) bool {
	return p[i].when.Before(p[j].when)
}

func (p remoteTargetSlice) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

type GitMode struct{}

func (g *GitMode) RetrieveSource(pd *data.ProcessData) *data.ModeData {
	repo, err := git.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		log.Fatalf("could not init git Repo: %v", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		log.Fatalf("could not get Worktree: %v", err)
	}

	refspec := config.RefSpec("+refs/heads/*:refs/remotes/*")
	remote, err := repo.CreateRemote(&config.RemoteConfig{
		Name:  "upstream",
		URLs:  []string{pd.RpmLocation},
		Fetch: []config.RefSpec{refspec},
	})
	if err != nil {
		log.Fatalf("could not create remote: %v", err)
	}

	err = remote.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{refspec},
		Tags:     git.AllTags,
		Force:    true,
	})
	if err != nil {
		log.Fatalf("could not fetch upstream: %v", err)
	}

	var branches remoteTargetSlice

	latestTags := map[string]*remoteTarget{}

	tagAdd := func(tag *object.Tag) error {
		if strings.HasPrefix(tag.Name, fmt.Sprintf("imports/%s%d", pd.ImportBranchPrefix, pd.Version)) {
			refSpec := fmt.Sprintf("refs/tags/%s", tag.Name)
			if tagImportRegex.MatchString(refSpec) {
				match := tagImportRegex.FindStringSubmatch(refSpec)

				exists := latestTags[match[2]]
				if exists != nil && exists.when.After(tag.Tagger.When) {
					return nil
				}

				latestTags[match[2]] = &remoteTarget{
					remote: refSpec,
					when:   tag.Tagger.When,
				}
			}
		}
		return nil
	}

	tagIter, err := repo.TagObjects()
	if err != nil {
		log.Fatalf("could not get tag objects: %v", err)
	}
	_ = tagIter.ForEach(tagAdd)

	if len(latestTags) == 0 {
		list, err := remote.List(&git.ListOptions{})
		if err != nil {
			log.Fatalf("could not list upstream: %v", err)
		}

		for _, ref := range list {
			if ref.Hash().IsZero() {
				continue
			}

			commit, err := repo.CommitObject(ref.Hash())
			if err != nil {
				log.Printf("could not get commit object for ref %s: %v", ref.Name().String(), err)
				continue
			}
			_ = tagAdd(&object.Tag{
				Name:   strings.TrimPrefix(string(ref.Name()), "refs/tags/"),
				Tagger: commit.Committer,
			})
		}
	}

	for _, branch := range latestTags {
		log.Printf("tag: %s", strings.TrimPrefix(branch.remote, "refs/tags/"))
		branches = append(branches, *branch)
	}

	sort.Sort(branches)

	var sortedBranches []string
	for _, branch := range branches {
		sortedBranches = append(sortedBranches, branch.remote)
	}

	return &data.ModeData{
		Repo:       repo,
		Worktree:   w,
		RpmFile:    createPackageFile(filepath.Base(pd.RpmLocation)),
		FileWrites: nil,
		Branches:   sortedBranches,
	}
}

func (g *GitMode) WriteSource(pd *data.ProcessData, md *data.ModeData) {
	remote, err := md.Repo.Remote("upstream")
	if err != nil {
		log.Fatalf("could not get upstream remote: %v", err)
	}

	var refspec config.RefSpec
	var branchName string

	if strings.HasPrefix(md.TagBranch, "refs/heads") {
		refspec = config.RefSpec(fmt.Sprintf("+%s:%s", md.TagBranch, md.TagBranch))
		branchName = strings.TrimPrefix(md.TagBranch, "refs/heads/")
	} else {
		match := tagImportRegex.FindStringSubmatch(md.TagBranch)
		branchName = match[2]
		refspec = config.RefSpec(fmt.Sprintf("+refs/heads/%s:%s", branchName, md.TagBranch))
	}
	log.Printf("checking out upstream refspec %s", refspec)
	err = remote.Fetch(&git.FetchOptions{
		RemoteName: "upstream",
		RefSpecs:   []config.RefSpec{refspec},
		Tags:       git.AllTags,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		log.Fatalf("could not fetch upstream: %v", err)
	}

	err = md.Worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.ReferenceName(md.TagBranch),
		Force:  true,
	})
	if err != nil {
		log.Fatalf("could not checkout source from git: %v", err)
	}

	_, err = md.Worktree.Add(".")
	if err != nil {
		log.Fatalf("could not add Worktree: %v", err)
	}

	metadataFile, err := md.Worktree.Filesystem.Open(fmt.Sprintf(".%s.metadata", md.RpmFile.Name()))
	if err != nil {
		log.Printf("warn: could not open metadata file, so skipping: %v", err)
		return
	}

	fileBytes, err := ioutil.ReadAll(metadataFile)
	if err != nil {
		log.Fatalf("could not read metadata file: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DisableCompression: false,
		},
	}
	fileContent := strings.Split(string(fileBytes), "\n")
	for _, line := range fileContent {
		if strings.TrimSpace(line) == "" {
			continue
		}

		lineInfo := strings.SplitN(line, " ", 2)
		hash := strings.TrimSpace(lineInfo[0])
		path := strings.TrimSpace(lineInfo[1])

		var body []byte

		if md.BlobCache[hash] != nil {
			body = md.BlobCache[hash]
			log.Printf("retrieving %s from cache", hash)
		} else {
			fromBlobStorage := pd.BlobStorage.Read(hash)
			if fromBlobStorage != nil && !pd.NoStorageDownload {
				body = fromBlobStorage
				log.Printf("downloading %s from blob storage", hash)
			} else {
				url := fmt.Sprintf("https://git.centos.org/sources/%s/%s/%s", md.RpmFile.Name(), branchName, hash)
				log.Printf("downloading %s", url)

				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					log.Fatalf("could not create new http request: %v", err)
				}
				req.Header.Set("Accept-Encoding", "*")

				resp, err := client.Do(req)
				if err != nil {
					log.Fatalf("could not download dist-git file: %v", err)
				}

				body, err = ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Fatalf("could not read the whole dist-git file: %v", err)
				}
				err = resp.Body.Close()
				if err != nil {
					log.Fatalf("could not close body handle: %v", err)
				}
			}

			md.BlobCache[hash] = body
		}

		f, err := md.Worktree.Filesystem.Create(path)
		if err != nil {
			log.Fatalf("could not open file pointer: %v", err)
		}

		hasher := CompareHash(body, hash)
		if hasher == nil {
			log.Fatal("checksum in metadata does not match dist-git file")
		}

		md.SourcesToIgnore = append(md.SourcesToIgnore, &data.IgnoredSource{
			Name:         path,
			HashFunction: hasher,
		})

		_, err = f.Write(body)
		if err != nil {
			log.Fatalf("could not copy dist-git file to in-tree: %v", err)
		}
		_ = f.Close()
	}
}

func (g *GitMode) PostProcess(md *data.ModeData) {
	for _, source := range md.SourcesToIgnore {
		_, err := md.Worktree.Filesystem.Stat(source.Name)
		if err == nil {
			err := md.Worktree.Filesystem.Remove(source.Name)
			if err != nil {
				log.Fatalf("could not remove dist-git file: %v", err)
			}
		}
	}

	_, err := md.Worktree.Add(".")
	if err != nil {
		log.Fatalf("could not add git sources: %v", err)
	}
}

func (g *GitMode) ImportName(_ *data.ProcessData, md *data.ModeData) string {
	if tagImportRegex.MatchString(md.TagBranch) {
		match := tagImportRegex.FindStringSubmatch(md.TagBranch)
		return match[3]
	}

	return strings.TrimPrefix(md.TagBranch, "refs/heads/")
}
