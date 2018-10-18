package main

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pkg/errors"
	"github.com/sourcegraph/docsite"
	"golang.org/x/tools/godoc/vfs/httpfs"
	"golang.org/x/tools/godoc/vfs/zipfs"
)

func siteFromFlags() (*docsite.Site, *docsiteConfig, error) {
	// First, check if this executable was built with `docsite build` and has site data bundled.
	if executable, err := os.Executable(); err == nil {
		site, config, err := openDocsiteFromELF(executable)
		if site != nil || err != nil {
			return site, config, err
		}
	}

	paths := filepath.SplitList(*configPath)
	for _, path := range paths {
		data, err := ioutil.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, nil, errors.WithMessage(err, "reading docsite config file (from -config flag)")
		}
		return openDocsiteFromConfig(data)
	}
	return nil, nil, fmt.Errorf("no docsite.json config file found (search paths: %s)", *configPath)
}

// docsiteConfig is the shape of docsite.json.
type docsiteConfig struct {
	Templates         string
	Content           string
	BaseURLPath       string
	Assets            string
	AssetsBaseURLPath string
	Check             struct {
		IgnoreURLPattern string
	}

	IsELF bool
}

func partialSiteFromConfig(config docsiteConfig) (*docsite.Site, error) {
	var checkIgnoreURLPattern *regexp.Regexp
	if config.Check.IgnoreURLPattern != "" {
		var err error
		checkIgnoreURLPattern, err = regexp.Compile(config.Check.IgnoreURLPattern)
		if err != nil {
			return nil, err
		}
	}

	return &docsite.Site{
		Base:                  &url.URL{Path: config.BaseURLPath},
		AssetsBase:            &url.URL{Path: config.AssetsBaseURLPath},
		CheckIgnoreURLPattern: checkIgnoreURLPattern,
	}, nil
}

// openDocsiteFromConfig reads the documentation site data from a docsite.json file.
func openDocsiteFromConfig(configData []byte) (*docsite.Site, *docsiteConfig, error) {
	var config docsiteConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, nil, errors.WithMessage(err, "reading docsite configuration")
	}

	site, err := partialSiteFromConfig(config)
	if err != nil {
		return nil, nil, err
	}

	httpDirOrNil := func(dir string) http.FileSystem {
		if dir == "" {
			return nil
		}
		return http.Dir(dir)
	}
	site.Templates = httpDirOrNil(config.Templates)
	site.Content = httpDirOrNil(config.Content)
	site.Assets = httpDirOrNil(config.Assets)
	return site, &config, nil
}

// openDocsiteFromELF reads the documentation site data from the ELF file at path.
func openDocsiteFromELF(path string) (*docsite.Site, *docsiteConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	elfFile, err := elf.NewFile(f)
	if err != nil {
		return nil, nil, err
	}
	defer elfFile.Close()

	readSection := func(name string) ([]byte, error) {
		for _, section := range elfFile.Sections {
			if section.Name == name {
				return section.Data()
			}
		}
		return nil, nil
	}

	// Read docsite.json.
	configData, err := readSection("docsite_config")
	if configData == nil && err == nil {
		// This ELF file does not appear to be built with `docsite build`.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var config docsiteConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, nil, err
	}

	// Read site data.
	sectionFileSystem := func(name string) (http.FileSystem, error) {
		data, err := readSection(name)
		if err != nil {
			return nil, err
		}
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		return slashPrefixFileSystem{httpfs.New(zipfs.New(&zip.ReadCloser{Reader: *z}, name))}, nil
	}
	assets, err := sectionFileSystem("docsite_assets")
	if err != nil {
		return nil, nil, errors.WithMessage(err, "reading assets from ELF")
	}
	templates, err := sectionFileSystem("docsite_templates")
	if err != nil {
		return nil, nil, errors.WithMessage(err, "reading templates from ELF")
	}
	content, err := sectionFileSystem("docsite_content")
	if err != nil {
		return nil, nil, errors.WithMessage(err, "reading content from ELF")
	}

	site, err := partialSiteFromConfig(config)
	if err != nil {
		return nil, nil, err
	}
	site.Templates = templates
	site.Content = content
	site.Assets = assets
	return site, &config, nil
}

type slashPrefixFileSystem struct {
	fs http.FileSystem
}

func (fs slashPrefixFileSystem) Open(name string) (http.File, error) {
	return fs.fs.Open("/" + name)
}