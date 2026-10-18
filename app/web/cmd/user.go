package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

func (a *application) userCommand() *cobra.Command {
	users := &cobra.Command{Use: "user", Short: "Manage control-plane users"}
	create := &cobra.Command{Use: "create-admin", Short: "Create an administrator without stopping serve",
		Long: "Read a password twice without echo from a terminal, or one line from stdin with --password-stdin. Never accepts a command-line password.",
		Args: cobra.NoArgs, RunE: a.createAdmin}
	create.Flags().String("username", "", "Administrator username (3–64 ASCII letters, digits, dots, underscores, hyphens)")
	create.Flags().Bool("password-stdin", false, "Read one password line from stdin")
	users.AddCommand(create)
	return users
}

func (a *application) createAdmin(command *cobra.Command, _ []string) (retErr error) {
	config, err := a.config.load()
	if err != nil {
		return err
	}
	username, err := command.Flags().GetString("username")
	if err != nil {
		return err
	}
	if err = auth.ValidateUsername(username); err != nil {
		return err
	}
	database, err := store.OpenCurrent(command.Context(), config.MetadataDir)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, database)
	password, err := readAdminPassword(command)
	if err != nil {
		return err
	}
	passwords, err := auth.NewPasswords(auth.DefaultPasswordParams())
	if err != nil {
		return err
	}
	hash, err := passwords.Hash(password)
	if err != nil {
		return err
	}
	user, err := database.CreateAdmin(command.Context(), username, hash)
	if err != nil {
		return err
	}
	return a.emit(struct {
		ID string `json:"id"`
	}{ID: user.ID})
}

func readAdminPassword(command *cobra.Command) (string, error) {
	fromStdin, err := command.Flags().GetBool("password-stdin")
	if err != nil {
		return "", err
	}
	if fromStdin {
		return readPasswordLine(command.InOrStdin())
	}
	input, ok := command.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(input.Fd())) {
		return "", errors.New("stdin is not a terminal; use --password-stdin")
	}
	first, err := promptPassword(command.ErrOrStderr(), input, "Password: ")
	if err != nil {
		return "", err
	}
	second, err := promptPassword(command.ErrOrStderr(), input, "Confirm password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("password confirmation does not match")
	}
	return first, nil
}

func readPasswordLine(input io.Reader) (string, error) {
	// At most 256 UTF-8 runes, plus a line ending. Any longer prefix still
	// exceeds the rune limit and is rejected, without unbounded stdin buffering.
	line, err := bufio.NewReader(io.LimitReader(input, 4*256+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("cannot read password from stdin")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func promptPassword(output io.Writer, input *os.File, prompt string) (string, error) {
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", err
	}
	data, err := term.ReadPassword(int(input.Fd()))
	if err != nil {
		return "", errors.New("cannot read password from terminal")
	}
	defer clear(data)
	if _, err = fmt.Fprintln(output); err != nil {
		return "", err
	}
	return string(data), nil
}
