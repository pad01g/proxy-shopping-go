// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";

import {Safe} from "@safe/Safe.sol";
import {SafeL2} from "@safe/SafeL2.sol";
import {Enum} from "@safe/common/Enum.sol";
import {SafeProxyFactory} from "@safe/proxies/SafeProxyFactory.sol";
import {CompatibilityFallbackHandler} from "@safe/handler/CompatibilityFallbackHandler.sol";
import {MultiSendCallOnly} from "@safe/libraries/MultiSendCallOnly.sol";

import {MockUSDC} from "../src/mocks/MockUSDC.sol";
import {PSEscrowModule} from "../src/PSEscrowModule.sol";
import {PSSafeSetup} from "../src/PSSafeSetup.sol";

/// spec §6 の流れ: 注文ごとの Safe を作り、入金し、2-of-3 / タイムロックで払い出す。
contract PSSafeFlowTest is Test {
    SafeL2 singleton;
    SafeProxyFactory factory;
    CompatibilityFallbackHandler fallbackHandler;
    MultiSendCallOnly multiSend;
    PSEscrowModule module;
    PSSafeSetup setupLib;
    MockUSDC usdc;

    uint256 constant U_KEY = 0xA11CE;
    uint256 constant S_KEY = 0xB0B;
    uint256 constant E_KEY = 0xE5C;
    address U;
    address S;
    address E;

    bytes16 constant ORDER_ID = bytes16(0x00112233445566778899aabbccddeeff);
    uint256 constant LOCK_AMOUNT = 86_600_000; // 86.6 USDC
    uint64 t1;
    uint64 t2;

    function setUp() public {
        singleton = new SafeL2();
        factory = new SafeProxyFactory();
        fallbackHandler = new CompatibilityFallbackHandler();
        multiSend = new MultiSendCallOnly();
        module = new PSEscrowModule();
        setupLib = new PSSafeSetup();
        usdc = new MockUSDC();

        U = vm.addr(U_KEY);
        S = vm.addr(S_KEY);
        E = vm.addr(E_KEY);
        vm.warp(1_790_000_000);
        t1 = uint64(block.timestamp + 3600);
        t2 = uint64(block.timestamp + 7200);
    }

    // ---- 組み立て ----

    function _initializer() internal view returns (bytes memory) {
        address[] memory owners = new address[](3);
        owners[0] = U;
        owners[1] = S;
        owners[2] = E;
        return abi.encodeCall(
            Safe.setup,
            (
                owners,
                2,
                address(setupLib),
                abi.encodeCall(PSSafeSetup.setup, (address(module), address(usdc), U, S, t1, t2)),
                address(fallbackHandler),
                address(0),
                0,
                payable(address(0))
            )
        );
    }

    function _saltNonce() internal pure returns (uint256) {
        return uint256(keccak256(abi.encodePacked(ORDER_ID)));
    }

    /// README に書いたオフラインの計算と同じもの。
    function _predictSafe() internal view returns (address) {
        bytes32 salt = keccak256(abi.encodePacked(keccak256(_initializer()), _saltNonce()));
        bytes memory initCode = abi.encodePacked(factory.proxyCreationCode(), uint256(uint160(address(singleton))));
        return vm.computeCreate2Address(salt, keccak256(initCode), address(factory));
    }

    function _createFundedSafe() internal returns (Safe safe) {
        safe = Safe(payable(address(factory.createProxyWithNonce(address(singleton), _initializer(), _saltNonce()))));
        usdc.mint(address(safe), LOCK_AMOUNT);
    }

    function _txHash(Safe safe, address to, bytes memory data, Enum.Operation op) internal view returns (bytes32) {
        uint256 nonce = safe.nonce();
        return safe.getTransactionHash(to, 0, data, op, 0, 0, 0, address(0), payable(address(0)), nonce);
    }

    /// EIP-712 の署名を、署名者のアドレス昇順に連結する。
    function _sign(Safe safe, address to, bytes memory data, Enum.Operation op, uint256 keyA, uint256 keyB)
        internal
        view
        returns (bytes memory)
    {
        bytes32 txHash = _txHash(safe, to, data, op);
        if (vm.addr(keyA) > vm.addr(keyB)) (keyA, keyB) = (keyB, keyA);
        return abi.encodePacked(_sig(keyA, txHash), _sig(keyB, txHash));
    }

    function _sig(uint256 key, bytes32 digest) internal pure returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(key, digest);
        return abi.encodePacked(r, s, v);
    }

    function _exec(Safe safe, address to, bytes memory data, Enum.Operation op, bytes memory sigs)
        internal
        returns (bool)
    {
        return safe.execTransaction(to, 0, data, op, 0, 0, 0, address(0), payable(address(0)), sigs);
    }

    // ---- テスト ----

    function test_createSafe_matchesOfflineAddressAndRegisters() public {
        address predicted = _predictSafe();
        Safe safe = _createFundedSafe();
        assertEq(address(safe), predicted);

        assertEq(safe.getThreshold(), 2);
        address[] memory owners = safe.getOwners();
        assertEq(owners.length, 3);
        assertEq(owners[0], U);
        assertEq(owners[1], S);
        assertEq(owners[2], E);
        assertTrue(safe.isModuleEnabled(address(module)));

        (address token, address user, address shopper, uint64 ct1, uint64 ct2) = module.config(address(safe));
        assertEq(token, address(usdc));
        assertEq(user, U);
        assertEq(shopper, S);
        assertEq(ct1, t1);
        assertEq(ct2, t2);
    }

    function test_register_onlyOnce() public {
        Safe safe = _createFundedSafe();
        vm.prank(address(safe));
        vm.expectRevert(PSEscrowModule.AlreadyRegistered.selector);
        module.register(address(usdc), U, S, t1, t2);
    }

    function test_setup_rejectsDirectCall() public {
        vm.expectRevert(PSSafeSetup.OnlyDelegateCall.selector);
        setupLib.setup(address(module), address(usdc), U, S, t1, t2);
    }

    function test_release_2of3() public {
        Safe safe = _createFundedSafe();
        bytes memory data = abi.encodeCall(MockUSDC.transfer, (S, LOCK_AMOUNT));
        bytes memory sigs = _sign(safe, address(usdc), data, Enum.Operation.Call, U_KEY, S_KEY);

        vm.prank(S); // 受け取る側がガス代を払う
        assertTrue(_exec(safe, address(usdc), data, Enum.Operation.Call, sigs));
        assertEq(usdc.balanceOf(S), LOCK_AMOUNT);
        assertEq(usdc.balanceOf(address(safe)), 0);
        assertEq(safe.nonce(), 1);
    }

    function test_release_rejectsSingleSignature() public {
        Safe safe = _createFundedSafe();
        bytes memory data = abi.encodeCall(MockUSDC.transfer, (S, LOCK_AMOUNT));
        bytes memory sig = _sig(S_KEY, _txHash(safe, address(usdc), data, Enum.Operation.Call));
        vm.expectRevert(bytes("GS020"));
        _exec(safe, address(usdc), data, Enum.Operation.Call, sig);
    }

    function test_disputeSplit_viaMultiSendDelegateCall() public {
        Safe safe = _createFundedSafe();
        uint256 toUser = 40_000_000;
        uint256 toEscrow = 2_000_000;
        uint256 toShopper = LOCK_AMOUNT - toUser - toEscrow;

        bytes memory batch = abi.encodePacked(
            _multiSendEntry(address(usdc), abi.encodeCall(MockUSDC.transfer, (U, toUser))),
            _multiSendEntry(address(usdc), abi.encodeCall(MockUSDC.transfer, (S, toShopper))),
            _multiSendEntry(address(usdc), abi.encodeCall(MockUSDC.transfer, (E, toEscrow)))
        );
        bytes memory data = abi.encodeCall(MultiSendCallOnly.multiSend, (batch));
        // escrow が署名し、user が連署して実行する
        bytes memory sigs = _sign(safe, address(multiSend), data, Enum.Operation.DelegateCall, E_KEY, U_KEY);

        vm.prank(U);
        assertTrue(_exec(safe, address(multiSend), data, Enum.Operation.DelegateCall, sigs));
        assertEq(usdc.balanceOf(U), toUser);
        assertEq(usdc.balanceOf(S), toShopper);
        assertEq(usdc.balanceOf(E), toEscrow);
        assertEq(usdc.balanceOf(address(safe)), 0);
    }

    function _multiSendEntry(address to, bytes memory data) internal pure returns (bytes memory) {
        return abi.encodePacked(uint8(0), to, uint256(0), uint256(data.length), data);
    }

    function test_claimByShopper_afterT1() public {
        Safe safe = _createFundedSafe();

        vm.warp(t1 - 1);
        vm.prank(S);
        vm.expectRevert(PSEscrowModule.TooEarly.selector);
        module.claimByShopper(address(safe));

        vm.warp(t1);
        vm.prank(U);
        vm.expectRevert(PSEscrowModule.Unauthorized.selector);
        module.claimByShopper(address(safe));

        vm.prank(E);
        vm.expectRevert(PSEscrowModule.Unauthorized.selector);
        module.claimByShopper(address(safe));

        vm.expectEmit(address(module));
        emit PSEscrowModule.ClaimedByShopper(address(safe), S, LOCK_AMOUNT);
        vm.prank(S);
        module.claimByShopper(address(safe));
        assertEq(usdc.balanceOf(S), LOCK_AMOUNT);
        assertEq(usdc.balanceOf(address(safe)), 0);
    }

    function test_refundToUser_afterT2() public {
        Safe safe = _createFundedSafe();

        // t1 を過ぎても t2 までは user は取り戻せない
        vm.warp(t2 - 1);
        vm.prank(U);
        vm.expectRevert(PSEscrowModule.TooEarly.selector);
        module.refundToUser(address(safe));

        vm.warp(t2);
        vm.prank(S);
        vm.expectRevert(PSEscrowModule.Unauthorized.selector);
        module.refundToUser(address(safe));

        vm.expectEmit(address(module));
        emit PSEscrowModule.RefundedToUser(address(safe), U, LOCK_AMOUNT);
        vm.prank(U);
        module.refundToUser(address(safe));
        assertEq(usdc.balanceOf(U), LOCK_AMOUNT);
    }

    function test_claim_unknownSafeReverts() public {
        vm.prank(S);
        vm.expectRevert(PSEscrowModule.NotRegistered.selector);
        module.claimByShopper(address(0xdead));
    }
}
