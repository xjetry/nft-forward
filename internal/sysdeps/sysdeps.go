package sysdeps

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// pkgCommands maps an apt package name to a binary whose presence proves the
// package is installed.
var pkgCommands = map[string]string{
	"nftables": "nft",
	"iproute2": "tc",
}

// Ensure installs missing packages via apt-get non-interactively. It is a
// no-op when all probed binaries are already on PATH. Requires root.
//
// On non-Debian systems (no apt-get) it returns a clear error so the operator
// can install manually.
func Ensure(pkgs ...string) error {
	missing := pkgs[:0:0]
	for _, pkg := range pkgs {
		cmd, ok := pkgCommands[pkg]
		if !ok {
			return fmt.Errorf("sysdeps: 未知依赖包名 %q", pkg)
		}
		if _, err := exec.LookPath(cmd); err != nil {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("缺少依赖 (%s) 且当前非 root，无法自动安装", strings.Join(missing, " "))
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return fmt.Errorf("缺少依赖 (%s)，且未找到 apt-get；请用系统包管理器手动安装", strings.Join(missing, " "))
	}

	fmt.Fprintf(os.Stderr, "[sysdeps] 自动安装缺失依赖: %s\n", strings.Join(missing, " "))

	env := map[string]string{"DEBIAN_FRONTEND": "noninteractive"}
	_ = run(env, "apt-get", "update")
	args := append([]string{"install", "-y", "--no-install-recommends"}, missing...)
	if err := run(env, "apt-get", args...); err != nil {
		// apt may fail for reasons unrelated to the package (e.g. interrupted
		// dpkg). Check whether the needed binaries landed anyway before giving up.
		allOK := true
		for _, pkg := range missing {
			if _, lerr := exec.LookPath(pkgCommands[pkg]); lerr != nil {
				allOK = false
				break
			}
		}
		if !allOK {
			return fmt.Errorf("apt-get install %s: %w", strings.Join(missing, " "), err)
		}
		fmt.Fprintf(os.Stderr, "[sysdeps] apt-get 报错但依赖已就绪，继续\n")
	}

	// Loading nf_tables may be required on minimal images where the module
	// is on disk but not auto-loaded.
	for _, p := range missing {
		if p == "nftables" {
			_ = exec.Command("modprobe", "nf_tables").Run()
		}
	}

	for _, pkg := range missing {
		if _, err := exec.LookPath(pkgCommands[pkg]); err != nil {
			return fmt.Errorf("安装后仍找不到 %s（来自 %s）", pkgCommands[pkg], pkg)
		}
	}
	return nil
}

func run(env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	envSlice := os.Environ()
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	cmd.Env = envSlice
	return cmd.Run()
}
