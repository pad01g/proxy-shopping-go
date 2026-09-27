// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";

import {MockUSDC} from "../src/mocks/MockUSDC.sol";
import {IERC20} from "../src/interfaces/IERC20.sol";
import {PSBond} from "../examples/bond/PSBond.sol";

contract PSBondTest is Test {
    MockUSDC usdc;
    PSBond bond;

    address operator = makeAddr("operator");
    address escrow = makeAddr("escrow");
    address victim = makeAddr("victim");
    uint64 constant DELAY = 7 days;
    uint64 constant WINDOW = 2 days;

    function setUp() public {
        usdc = new MockUSDC();
        bond = new PSBond(IERC20(address(usdc)), operator, DELAY, WINDOW);
        usdc.mint(escrow, 1_000e6);
        vm.prank(escrow);
        usdc.approve(address(bond), type(uint256).max);
    }

    function test_depositAndSlash() public {
        vm.expectEmit(address(bond));
        emit PSBond.Deposited(escrow, 500e6, 500e6);
        vm.prank(escrow);
        bond.deposit(500e6);
        assertEq(bond.bondOf(escrow), 500e6);

        vm.prank(escrow);
        vm.expectRevert(PSBond.Unauthorized.selector);
        bond.slash(escrow, victim, 1);

        vm.prank(operator);
        vm.expectRevert(PSBond.InsufficientBond.selector);
        bond.slash(escrow, victim, 500e6 + 1);

        vm.expectEmit(address(bond));
        emit PSBond.Slashed(escrow, victim, 200e6, 300e6);
        vm.prank(operator);
        bond.slash(escrow, victim, 200e6);
        assertEq(usdc.balanceOf(victim), 200e6);
        assertEq(bond.bondOf(escrow), 300e6);
    }

    function test_withdrawAfterDelay() public {
        vm.startPrank(escrow);
        bond.deposit(500e6);
        vm.expectRevert(PSBond.NotRequested.selector);
        bond.withdraw();
        bond.requestWithdraw();
        vm.expectRevert(PSBond.TooEarly.selector);
        bond.withdraw();
        vm.stopPrank();

        vm.warp(block.timestamp + DELAY);
        vm.expectEmit(address(bond));
        emit PSBond.Withdrawn(escrow, 500e6);
        vm.prank(escrow);
        bond.withdraw();
        assertEq(usdc.balanceOf(escrow), 1_000e6);
        assertEq(bond.bondOf(escrow), 0);
    }

    /// an old request cannot be kept around to withdraw right after a bad ruling
    function test_requestExpiresAfterWindow() public {
        vm.startPrank(escrow);
        bond.deposit(500e6);
        bond.requestWithdraw();
        vm.warp(block.timestamp + DELAY + WINDOW + 1);
        vm.expectRevert(PSBond.TooLate.selector);
        bond.withdraw();
        // a new request starts the delay again
        bond.requestWithdraw();
        vm.expectRevert(PSBond.TooEarly.selector);
        bond.withdraw();
        vm.warp(block.timestamp + DELAY + WINDOW);
        bond.withdraw();
        vm.stopPrank();
        assertEq(bond.bondOf(escrow), 0);
    }

    /// a slash cancels the pending request: the rest stays while the report is looked into
    function test_slashCancelsRequest() public {
        vm.startPrank(escrow);
        bond.deposit(500e6);
        bond.requestWithdraw();
        vm.stopPrank();

        vm.expectEmit(address(bond));
        emit PSBond.WithdrawCancelled(escrow);
        vm.prank(operator);
        bond.slash(escrow, victim, 100e6);
        assertEq(bond.withdrawableAt(escrow), 0);

        vm.warp(block.timestamp + DELAY);
        vm.prank(escrow);
        vm.expectRevert(PSBond.NotRequested.selector);
        bond.withdraw();
        assertEq(bond.bondOf(escrow), 400e6);
    }

    /// a deposit cancels the pending request: the new money waits the full delay too
    function test_depositCancelsRequest() public {
        vm.startPrank(escrow);
        bond.deposit(200e6);
        bond.requestWithdraw();
        vm.warp(block.timestamp + DELAY - 1);
        bond.deposit(300e6);
        assertEq(bond.withdrawableAt(escrow), 0);
        vm.warp(block.timestamp + 1);
        vm.expectRevert(PSBond.NotRequested.selector);
        bond.withdraw();
        vm.stopPrank();
    }
}
