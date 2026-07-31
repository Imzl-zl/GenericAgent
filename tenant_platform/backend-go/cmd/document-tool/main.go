// Command document-tool is the only executable shipped in the document image.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const (
	maxRequestBytes    = 1024*1024 + 8*1024
	maxContentBytes    = 1024 * 1024
	maxTitleBytes      = 4096
	maxDOCXOutputBytes = 8 * 1024 * 1024
)

type exportRequest struct {
	SchemaVersion int    `json:"schema_version"`
	Title         string `json:"title"`
	Content       string `json:"content"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var err error
	switch {
	case len(args) == 1 && args[0] == "idle":
		err = idle()
	case len(args) == 2 && args[0] == "read-cgroup":
		err = readCgroup(stdout, args[1])
	case len(args) == 5 && args[0] == "export-docx" && args[1] == "--input" && args[2] == "-" && args[3] == "--output" && args[4] == "-":
		err = exportDOCX(stdin, stdout)
	default:
		err = errors.New("unsupported document tool command")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func idle() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	<-stop
	return nil
}

func readCgroup(stdout io.Writer, name string) error {
	path, err := cgroupPath(name)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read cgroup value: %w", err)
	}
	if len(content) == 0 || len(content) > 128 {
		return errors.New("cgroup value size is invalid")
	}
	_, err = stdout.Write(content)
	return err
}

func cgroupPath(name string) (string, error) {
	switch name {
	case "memory.max":
		return "/sys/fs/cgroup/memory.max", nil
	case "cpu.max":
		return "/sys/fs/cgroup/cpu.max", nil
	case "pids.max":
		return "/sys/fs/cgroup/pids.max", nil
	default:
		return "", errors.New("unsupported cgroup value")
	}
}

func exportDOCX(stdin io.Reader, stdout io.Writer) error {
	limited := io.LimitReader(stdin, maxRequestBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read export request: %w", err)
	}
	if len(raw) == 0 || len(raw) > maxRequestBytes {
		return errors.New("export request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request exportRequest
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode export request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("export request must contain one JSON object")
	}
	if request.SchemaVersion != 1 {
		return errors.New("export request schema_version must be 1")
	}
	if strings.TrimSpace(request.Content) == "" || len([]byte(request.Content)) > maxContentBytes || !validXMLText(request.Content) {
		return errors.New("export content must be non-empty, bounded, and valid XML text")
	}
	if len([]byte(request.Title)) > maxTitleBytes || !validXMLText(request.Title) {
		return errors.New("export title is too large or contains invalid XML characters")
	}
	content, err := buildDOCX(request)
	if err != nil {
		return err
	}
	if len(content) == 0 || len(content) > maxDOCXOutputBytes {
		return errors.New("generated document size is invalid")
	}
	_, err = stdout.Write(content)
	return err
}

func validXMLText(value string) bool {
	for _, r := range value {
		if r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF) {
			continue
		}
		return false
	}
	return true
}

func buildDOCX(request exportRequest) ([]byte, error) {
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/></Types>`,
		"_rels/.rels":         `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`,
		"word/styles.xml":     `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style></w:styles>`,
		"word/document.xml":   documentXML(request.Content),
		"docProps/core.xml":   coreXML(request.Title),
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "docProps/core.xml", "word/document.xml", "word/styles.xml"} {
		writer, err := archive.Create(name)
		if err != nil {
			return nil, fmt.Errorf("create DOCX entry: %w", err)
		}
		if _, err := io.WriteString(writer, files[name]); err != nil {
			return nil, fmt.Errorf("write DOCX entry: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close DOCX archive: %w", err)
	}
	return output.Bytes(), nil
}

func documentXML(content string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	blocks := strings.Split(normalized, "\n\n")
	var body strings.Builder
	for _, block := range blocks {
		body.WriteString(`<w:p>`)
		lines := strings.Split(block, "\n")
		for index, line := range lines {
			if index > 0 {
				body.WriteString(`<w:r><w:br/></w:r>`)
			}
			body.WriteString(`<w:r><w:t xml:space="preserve">`)
			body.WriteString(html.EscapeString(line))
			body.WriteString(`</w:t></w:r>`)
		}
		body.WriteString(`</w:p>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/></w:sectPr></w:body></w:document>`
}

func coreXML(title string) string {
	if strings.TrimSpace(title) == "" {
		title = "GenericAgent Export"
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>` + html.EscapeString(title) + `</dc:title><dc:creator>GenericAgent</dc:creator></cp:coreProperties>`
}
