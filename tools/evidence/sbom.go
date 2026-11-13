package main

import (
	"errors"
	"fmt"
	"strings"
)

// spdxDocument is the part of an SPDX 2.x JSON document that the bundle uses.
type spdxDocument struct {
	SPDXVersion  string `json:"spdxVersion"`
	Name         string `json:"name"`
	CreationInfo struct {
		Created  string   `json:"created"`
		Creators []string `json:"creators"`
	} `json:"creationInfo"`
	Packages []struct {
		SPDXID       string            `json:"SPDXID"`
		Name         string            `json:"name"`
		ExternalRefs []spdxExternalRef `json:"externalRefs"`
	} `json:"packages"`
}

type spdxExternalRef struct {
	ReferenceType    string `json:"referenceType"`
	ReferenceLocator string `json:"referenceLocator"`
}

type sbomData struct {
	Format       string   `json:"format"`
	DocumentName string   `json:"documentName"`
	Created      string   `json:"created"`
	Creators     []string `json:"creators"`
	Packages     int      `json:"packages"`
	// Packages per package-URL type (golang, github, ...); "other" for packages without a purl,
	// such as Terraform providers and container actions.
	ByType map[string]int `json:"byType"`
}

func parseSBOM(data []byte) (*sbomData, bool, string, error) {
	var doc spdxDocument
	if err := decode(data, &doc); err != nil {
		return nil, false, "", err
	}
	if !strings.HasPrefix(doc.SPDXVersion, "SPDX-2.") {
		return nil, false, "", errors.New("not an SPDX 2.x JSON document")
	}

	d := &sbomData{
		Format:       doc.SPDXVersion,
		DocumentName: doc.Name,
		Created:      doc.CreationInfo.Created,
		Creators:     doc.CreationInfo.Creators,
		ByType:       map[string]int{},
	}
	if d.Creators == nil {
		d.Creators = []string{}
	}
	for _, p := range doc.Packages {
		// syft describes the scanned directory itself as a package; it is not a dependency.
		if strings.HasPrefix(p.SPDXID, "SPDXRef-DocumentRoot") {
			continue
		}
		d.Packages++
		d.ByType[purlType(p.ExternalRefs)]++
	}

	if d.Packages == 0 {
		return d, true, "the SBOM lists no packages; check the syft step", nil
	}
	return d, false, fmt.Sprintf("%s, %s", d.Format, plural(d.Packages, "package", "packages")), nil
}

func purlType(refs []spdxExternalRef) string {
	for _, r := range refs {
		if r.ReferenceType != "purl" {
			continue
		}
		// pkg:golang/golang.org/x/net@v0.1.0 -> golang
		rest, ok := strings.CutPrefix(r.ReferenceLocator, "pkg:")
		if !ok {
			continue
		}
		if t, _, ok := strings.Cut(rest, "/"); ok && t != "" {
			return t
		}
	}
	return "other"
}
