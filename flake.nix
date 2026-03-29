{
  description = "A basic flake with a shell";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  inputs.systems.url = "github:nix-systems/default";
  inputs.flake-utils = {
    url = "github:numtide/flake-utils";
    inputs.systems.follows = "systems";
  };

  outputs =
    { nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        formatter = pkgs.nixfmt-tree;
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            bashInteractive
            # Kubernetes
            kubectl
            # Secret management
            sops
            age
            # Terraform
            opentofu
          ];
          env = {
            SOPS_AGE_KEY_CMD = "rbw get lepinoid-infra-age-key";
          };
          shellHook = ''
            export TF_VAR_state_encryption_passphrase=$(command -v rbw >/dev/null 2>&1 && rbw get lepinoid-infra-tf-state-encryption || echo "missing");
            export TF_VAR_cloudflare_api_token=$(command -v rbw >/dev/null 2>&1 && rbw get lepinoid-infra-cloudflare-api-token || echo "missing");
          '';
        };
      }
    );
}
