{
  description = "Dickord personal Discord to Ergo IRC bridge";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      dickord = pkgs.buildGoModule {
        pname = "dickord";
        version = "0.1.0";
        src = ./.;
        vendorHash = "sha256-kdP5lMOJlsfnsd7AiH1aZsZeZQCjING8gA0N1LnP7/A=";
        ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];
      };
    in {
      packages.${system}.default = dickord;
      checks.${system}.default = dickord;
      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [ go gopls gotools docker-client openssl ];
      };
    };
}
