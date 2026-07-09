package importer

import (
	"encoding/xml"
	"strings"
)

// domainDef is a minimal view of a libvirt domain XML — just enough to locate
// the UEFI NVRAM variables file. Whether a domain has NVRAM is driven purely by
// its firmware (UEFI vs BIOS), NOT by the guest OS: a UEFI Linux VM has one, a
// BIOS Windows VM does not. So there is no Windows-vs-Linux branching anywhere —
// we simply back up the NVRAM when the XML declares one.
type domainDef struct {
	OS struct {
		Nvram string `xml:"nvram"`
	} `xml:"os"`
}

// nvramPath returns the path to the domain's UEFI NVRAM vars file, or "" if the
// domain has no NVRAM (BIOS/SeaBIOS firmware). The <nvram> element's text is the
// per-VM vars file; its "template" attribute (the base firmware) is ignored.
func nvramPath(domainXML []byte) string {
	var d domainDef
	if err := xml.Unmarshal(domainXML, &d); err != nil {
		return ""
	}
	return strings.TrimSpace(d.OS.Nvram)
}
