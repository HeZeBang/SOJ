// Package deploy implements `soj import`: takes a problem-package directory
// (containing problem.yaml + Apptainer.def + scaffold files), builds the
// image, stages the scaffold to ScaffoldDir, renders the problem.yaml
// template, and writes it to ProblemsDir.
package deploy

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// renderVars supplies values to problem.yaml templates.
type renderVars struct {
	Image    string
	Scaffold string
	Id       string
}

// ImportPackage processes a problem package and deploys it.
//   - Reads <packageDir>/problem.yaml (must have id + package: section).
//   - If Package.Image.Def is set: builds <ImagesDir>/<id>.sif.
//   - If Package.Image.Sif is set: copies it to <ImagesDir>/<id>.sif.
//   - Copies each scaffold entry to <ScaffoldDir>/<id>/ with sane perms.
//   - Renders problem.yaml templates and writes <ProblemsDir>/<id>.yaml
//     (with the package: section stripped — runtime doesn't need it).
func ImportPackage(cfg *types.Config, packageDir string) error {
	packageDir, err := filepath.Abs(packageDir)
	if err != nil {
		return fmt.Errorf("resolve package dir: %w", err)
	}

	srcYaml := filepath.Join(packageDir, "problem.yaml")
	raw, err := os.ReadFile(srcYaml)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcYaml, err)
	}

	var p types.Problem
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse %s: %w", srcYaml, err)
	}
	if p.Id == "" {
		return fmt.Errorf("%s: missing id", srcYaml)
	}
	if err := requirePath(cfg.ImagesDir, "ImagesDir"); err != nil {
		return err
	}
	if err := requirePath(cfg.ScaffoldDir, "ScaffoldDir"); err != nil {
		return err
	}
	if err := requirePath(cfg.ProblemsDir, "ProblemsDir"); err != nil {
		return err
	}

	imagePath := filepath.Join(cfg.ImagesDir, p.Id+".sif")
	scaffoldPath := filepath.Join(cfg.ScaffoldDir, p.Id)

	log.Info().Str("id", p.Id).Str("from", packageDir).Msg("import: starting")

	if p.Package != nil && p.Package.Image != nil {
		if err := stageImage(packageDir, p.Package.Image, imagePath); err != nil {
			return fmt.Errorf("stage image: %w", err)
		}
	} else {
		log.Warn().Str("id", p.Id).Msg("import: no package.image specified, skipping image build")
	}

	if p.Package != nil && len(p.Package.Scaffold) > 0 {
		if err := stageScaffold(packageDir, p.Package.Scaffold, scaffoldPath); err != nil {
			return fmt.Errorf("stage scaffold: %w", err)
		}
	} else {
		log.Warn().Str("id", p.Id).Msg("import: no package.scaffold specified, skipping scaffold stage")
	}

	dstYaml := filepath.Join(cfg.ProblemsDir, p.Id+".yaml")
	if err := renderProblemYaml(raw, imagePath, scaffoldPath, p.Id, dstYaml); err != nil {
		return fmt.Errorf("render problem.yaml: %w", err)
	}

	log.Info().Str("id", p.Id).
		Str("image", imagePath).Str("scaffold", scaffoldPath).Str("problem", dstYaml).
		Msg("import: done")
	return nil
}

func requirePath(p, name string) error {
	if p == "" {
		return fmt.Errorf("config.%s is empty", name)
	}
	if err := os.MkdirAll(p, 0755); err != nil {
		return fmt.Errorf("mkdir %s (%s): %w", p, name, err)
	}
	return nil
}

func stageImage(packageDir string, spec *types.PackageImage, dst string) error {
	switch {
	case spec.Sif != "":
		src := filepath.Join(packageDir, spec.Sif)
		log.Info().Str("from", src).Str("to", dst).Msg("import: copying prebuilt SIF")
		return copyFile(src, dst, 0644)
	case spec.Def != "":
		def := filepath.Join(packageDir, spec.Def)
		log.Info().Str("def", def).Str("sif", dst).Msg("import: apptainer build")
		// --force overwrites existing SIF. Build runs as the invoker (root if
		// `soj import` is run with sudo, which it generally must be).
		cmd := exec.Command("apptainer", "build", "--force", dst, def)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("apptainer build: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("package.image needs either def or sif")
	}
}

func stageScaffold(packageDir string, entries []string, dst string) error {
	// Wipe target so removed entries actually disappear.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("rm %s: %w", dst, err)
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}

	for _, e := range entries {
		clean := strings.TrimSuffix(e, "/")
		if clean == "" || strings.Contains(clean, "..") {
			return fmt.Errorf("invalid scaffold entry %q", e)
		}
		src := filepath.Join(packageDir, clean)
		dstEntry := filepath.Join(dst, clean)
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("stat scaffold entry %s: %w", src, err)
		}
		if info.IsDir() {
			if err := copyDir(src, dstEntry); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(dstEntry), 0755); err != nil {
				return err
			}
			if err := copyFile(src, dstEntry, fileModeFor(clean, info.Mode())); err != nil {
				return err
			}
		}
		log.Info().Str("entry", e).Str("to", dstEntry).Msg("import: scaffold staged")
	}
	return nil
}

// fileModeFor picks 0755 for executable scripts (.sh) and files that were
// already +x in the package; 0644 for everything else. We deliberately
// re-normalize rather than honoring source mode entirely, so a stray
// world-writable file in the package can't become world-writable in the
// staged scaffold.
func fileModeFor(name string, srcMode os.FileMode) os.FileMode {
	if strings.HasSuffix(name, ".sh") || (srcMode&0111) != 0 {
		return 0755
	}
	return 0644
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Chmod again in case umask stripped bits.
	return os.Chmod(dst, mode)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		return copyFile(p, target, fileModeFor(info.Name(), info.Mode()))
	})
}

func renderProblemYaml(raw []byte, imagePath, scaffoldPath, id, dst string) error {
	tpl, err := template.New("problem").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, renderVars{
		Image:    imagePath,
		Scaffold: scaffoldPath,
		Id:       id,
	}); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}

	// Strip the package: section from the rendered output — runtime doesn't
	// need it and leaving it can confuse readers of /data/soj/problems/*.yaml.
	stripped, err := stripPackageSection(buf.Bytes())
	if err != nil {
		return err
	}

	if err := os.WriteFile(dst, stripped, 0644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

func stripPackageSection(b []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return nil, fmt.Errorf("re-parse rendered yaml: %w", err)
	}
	// Document → Mapping → drop the "package" key
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		root := node.Content[0]
		if root.Kind == yaml.MappingNode {
			out := make([]*yaml.Node, 0, len(root.Content))
			for i := 0; i+1 < len(root.Content); i += 2 {
				if root.Content[i].Value == "package" {
					continue
				}
				out = append(out, root.Content[i], root.Content[i+1])
			}
			root.Content = out
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}
