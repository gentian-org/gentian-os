package applifecycle
import (
    "os/exec"
)
func initGitRepo(root string) error {
    cmd := exec.Command("git", "init")
    cmd.Dir = root
    if err := cmd.Run(); err != nil {
        return err
    }
    cmd = exec.Command("git", "commit", "--allow-empty", "-m", "init")
    cmd.Dir = root
    return cmd.Run()
}
