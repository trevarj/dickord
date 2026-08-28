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
      python = pkgs.python3.withPackages (ps: [ ps.aiohttp ]);
      rdircdCheck = pkgs.runCommand "rdircd-check" {
        nativeBuildInputs = [ python ];
        src = ./rdircd;
      } ''
        cp -r "$src" rdircd
        chmod -R u+w rdircd
        python -m py_compile rdircd/rdircd
        PYTHONDONTWRITEBYTECODE=1 python -m unittest discover -s rdircd -p 'test_*.py'
        touch "$out"
      '';
    in {
      packages.${system}.default = dickord;
      checks.${system} = {
        default = dickord;
        rdircd = rdircdCheck;
      };
      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [ go gopls gotools docker-client openssl python ];
      };
    };
}
