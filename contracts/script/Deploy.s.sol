// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Script, console2} from "forge-std/Script.sol";

import {SafeL2} from "@safe/SafeL2.sol";
import {SafeProxyFactory} from "@safe/proxies/SafeProxyFactory.sol";
import {CompatibilityFallbackHandler} from "@safe/handler/CompatibilityFallbackHandler.sol";
import {MultiSendCallOnly} from "@safe/libraries/MultiSendCallOnly.sol";

import {MockUSDC} from "../src/mocks/MockUSDC.sol";
import {MockAggregatorV3} from "../src/mocks/MockAggregatorV3.sol";
import {PSEscrowModule} from "../src/PSEscrowModule.sol";
import {PSSafeSetup} from "../src/PSSafeSetup.sol";
import {PSBond} from "../examples/bond/PSBond.sol";
import {IERC20} from "../src/interfaces/IERC20.sol";

/// すべてを CREATE2 deployer（0x4e59…956c）から salt 0 で置く。
/// 既にコードがあるアドレスは飛ばすので、何度実行しても同じ結果になる。
/// 結果は deployments/<chain_id>.json に書く。
contract Deploy is Script {
    bytes32 internal constant SALT = bytes32(0);

    /// lab の operator-1（m/44'/60'/0'/0/0）
    address internal constant BOND_OPERATOR = 0xFDD9c12c4854FFeeB2C8AC9aEacEA1ce518afA14;
    uint64 internal constant BOND_WITHDRAW_DELAY = 7 days;
    uint64 internal constant BOND_WITHDRAW_WINDOW = 2 days;

    int256 internal constant BTC_USD = 100_000e8;
    int256 internal constant JPY_USD = 666_667; // 1/150 × 1e8 を四捨五入
    int256 internal constant USDC_USD = 1e8;

    struct Deployment {
        address usdc;
        address singleton;
        address factory;
        address fallbackHandler;
        address multiSendCallOnly;
        address module;
        address setup;
        address feedBtcUsd;
        address feedJpyUsd;
        address feedUsdcUsd;
        address bond;
    }

    function run() external returns (Deployment memory d) {
        require(CREATE2_FACTORY.code.length > 0, "CREATE2 deployer missing");
        vm.startBroadcast();

        d.usdc = _deploy(type(MockUSDC).creationCode);
        d.singleton = _deploy(type(SafeL2).creationCode);
        d.factory = _deploy(type(SafeProxyFactory).creationCode);
        d.fallbackHandler = _deploy(type(CompatibilityFallbackHandler).creationCode);
        d.multiSendCallOnly = _deploy(type(MultiSendCallOnly).creationCode);
        d.module = _deploy(type(PSEscrowModule).creationCode);
        d.setup = _deploy(type(PSSafeSetup).creationCode);
        d.feedBtcUsd = _deployFeed("BTC / USD", BTC_USD);
        d.feedJpyUsd = _deployFeed("JPY / USD", JPY_USD);
        d.feedUsdcUsd = _deployFeed("USDC / USD", USDC_USD);
        d.bond = _deploy(
            abi.encodePacked(type(PSBond).creationCode, abi.encode(IERC20(d.usdc), BOND_OPERATOR, BOND_WITHDRAW_DELAY, BOND_WITHDRAW_WINDOW))
        );

        vm.stopBroadcast();

        string memory path = string.concat(vm.projectRoot(), "/deployments/", vm.toString(block.chainid), ".json");
        vm.writeFile(path, _json(d));
        console2.log("wrote", path);
    }

    function _deployFeed(string memory description, int256 answer) internal returns (address) {
        return _deploy(abi.encodePacked(type(MockAggregatorV3).creationCode, abi.encode(description, answer)));
    }

    function _deploy(bytes memory initCode) internal returns (address addr) {
        addr = vm.computeCreate2Address(SALT, keccak256(initCode), CREATE2_FACTORY);
        if (addr.code.length > 0) return addr;
        (bool ok,) = CREATE2_FACTORY.call(abi.encodePacked(SALT, initCode));
        require(ok && addr.code.length > 0, "CREATE2 deploy failed");
    }

    // forgefmt: disable-start
    function _json(Deployment memory d) internal view returns (string memory) {
        return string.concat(
            "{\n",
            '  "chain_id": ', vm.toString(block.chainid), ",\n",
            _field("usdc", d.usdc), ",\n",
            '  "safe": {\n',
            "  ", _field("singleton", d.singleton), ",\n",
            "  ", _field("factory", d.factory), ",\n",
            "  ", _field("fallback_handler", d.fallbackHandler), ",\n",
            "  ", _field("multisend_call_only", d.multiSendCallOnly), "\n",
            "  },\n",
            _field("module", d.module), ",\n",
            _field("setup", d.setup), ",\n",
            '  "feeds": {\n',
            "  ", _field("BTC/USD", d.feedBtcUsd), ",\n",
            "  ", _field("JPY/USD", d.feedJpyUsd), ",\n",
            "  ", _field("USDC/USD", d.feedUsdcUsd), "\n",
            "  },\n",
            _field("bond", d.bond), "\n",
            "}\n"
        );
    }
    // forgefmt: disable-end

    function _field(string memory key, address value) internal pure returns (string memory) {
        return string.concat('  "', key, '": "', vm.toString(value), '"');
    }
}
