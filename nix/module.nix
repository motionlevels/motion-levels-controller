self: { config, lib, pkgs, ... }:
let
  cfg = config.services.motion-levels-floor-controller;
  types = lib.types;
  arguments = [
    "${cfg.package}/bin/motion-levels-controller"
    "-http" cfg.httpAddr
    "-engine" cfg.engineAddr
    "-recv-port" (toString cfg.recvPort)
    "-floor-rotation" (toString cfg.floorRotation)
    "-broadcast-ip" cfg.broadcastIP
  ]
  ++ lib.optionals (cfg.floorSourceIP != null) [ "-floor-source-ip" cfg.floorSourceIP ]
  ++ lib.optionals cfg.debugControls [ "-debug-controls" ]
  ++ cfg.extraArgs;
in {
  options.services.motion-levels-floor-controller = {
    enable = lib.mkEnableOption "Motion Levels floor controller service";

    package = lib.mkOption {
      type = types.package;
      default = if self ? packages.${pkgs.system}.default then self.packages.${pkgs.system}.default else pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.motion-levels-controller";
      description = "The motion-levels-controller package to run.";
    };
    httpAddr = lib.mkOption {
      type = types.str;
      default = "127.0.0.1:4200";
      description = "Loopback HTTP address for diagnostics, health, and metrics.";
    };
    engineAddr = lib.mkOption {
      type = types.str;
      default = "127.0.0.1:4201";
      description = "Loopback TCP address for the single engine stream.";
    };
    recvPort = lib.mkOption {
      type = types.port;
      default = 7800;
      description = "UDP port for floor handshake and sensor packets.";
    };
    floorSourceIP = lib.mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Exact local IPv4 source for floor UDP output; null uses the default route.";
    };
    floorRotation = lib.mkOption {
      type = types.enum [ 0 180 ];
      default = 0;
      description = "Logical-to-physical floor rotation in degrees.";
    };
    broadcastIP = lib.mkOption {
      type = types.str;
      default = "255.255.255.255";
      description = "Floor LED broadcast IPv4 address.";
    };
    debugControls = lib.mkOption {
      type = types.bool;
      default = false;
      description = "Enable loopback pressure simulation and statistics-reset endpoints.";
    };
    watchdogSec = lib.mkOption {
      type = types.nullOr types.str;
      default = "10s";
      description = "Systemd watchdog timeout; null disables watchdog heartbeats.";
    };
    extraArgs = lib.mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional controller CLI arguments.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.motion-levels-floor-controller = {
      description = "Motion Levels floor controller";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = lib.escapeShellArgs arguments;
        Restart = "always";
        RestartSec = "2s";
        TimeoutStartSec = "20s";

        DynamicUser = true;
        UMask = "0077";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectClock = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
      } // lib.optionalAttrs (cfg.watchdogSec != null) {
        WatchdogSec = cfg.watchdogSec;
      };
    };
  };
}
