package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brandonrc/bifrost/internal/auth"
)

func newExchangeCmd() *cobra.Command {
	var (
		issuer            string
		clientID          string
		clientSecret      string
		subjectToken      string
		subjectTokenStdin bool
		idToken           bool
		audience          string
		scope             string
	)
	cmd := &cobra.Command{
		Use: "exchange",
		Short: "Exchange a user's token for a Bifrost-audience token that carries the USER as subject (RFC 8693, #102). " +
			"Prints the exchanged access token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			secret := resolveClientSecret(clientSecret)
			if issuer == "" {
				return errors.New("required flag(s) \"issuer\" not set")
			}
			if clientID == "" {
				return errors.New("required flag(s) \"client-id\" not set")
			}
			if secret == "" {
				return errors.New("required flag(s) \"client-secret\" not set (or set BIFROST_CLIENT_SECRET)")
			}
			if subjectTokenStdin && subjectToken != "" {
				return errors.New("--subject-token and --subject-token-stdin are mutually exclusive")
			}
			var scopePtr *string
			if scope != "" {
				scopePtr = &scope
			}
			return runExchange(cmd.Context(), issuer, clientID, secret, subjectToken, subjectTokenStdin, idToken, audience, scopePtr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&issuer, "issuer", "", "OIDC issuer URL (its token endpoint performs the exchange)")
	f.StringVar(&clientID, "client-id", "", "The trusted service's confidential client id (e.g. checkmaite-svc)")
	// Default left empty deliberately — see resolveClientSecret's doc
	// comment for why BIFROST_CLIENT_SECRET is NOT wired in as the pflag
	// default here.
	f.StringVar(&clientSecret, "client-secret", "", "The service's client secret (or set BIFROST_CLIENT_SECRET)")
	f.StringVar(&subjectToken, "subject-token", "", "The user's token to exchange")
	f.BoolVar(&subjectTokenStdin, "subject-token-stdin", false, "Read the user's subject token from stdin (one line)")
	f.BoolVar(&idToken, "id-token", false, "Treat the subject token as an OIDC id token rather than an access token")
	f.StringVar(&audience, "audience", "bifrost", "Requested audience for the exchanged token")
	f.StringVar(&scope, "scope", "", "Optional requested scope")
	return cmd
}

// runExchange performs an RFC 8693 token exchange, ported from
// the predecessor CLI's exchange_user_token.
func runExchange(
	ctx context.Context,
	issuer, clientID, clientSecret, subjectToken string,
	subjectTokenStdin, idToken bool,
	audience string,
	scope *string,
) error {
	if subjectTokenStdin {
		t, err := readLineFromStdin()
		if err != nil {
			return err
		}
		subjectToken = t
	} else if subjectToken == "" {
		return errors.New("provide --subject-token or --subject-token-stdin")
	}

	client := auth.IdpClient()
	meta, err := auth.DiscoverMetadata(ctx, client, issuer)
	if err != nil {
		return err
	}
	if meta.TokenEndpoint == nil {
		return errors.New("issuer does not advertise a token_endpoint")
	}

	params := auth.NewTokenExchangeParams(clientID, clientSecret, subjectToken)
	if idToken {
		params.SubjectTokenType = auth.TokenTypeIDToken
	}
	params.Audience = &audience
	params.Scope = scope

	tok, err := auth.ExchangeToken(ctx, client, *meta.TokenEndpoint, params)
	if err != nil {
		return err
	}
	fmt.Println(tok.AccessToken)
	return nil
}
